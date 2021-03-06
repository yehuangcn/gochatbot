// +build all slack

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"cirello.io/gochatbot/messages"
)

const (
	slackEnvVarName = "GOCHATBOT_SLACK_TOKEN"
	urlSlackAPI     = "https://slack.com/api/"
)

func init() {
	availableProviders = append(availableProviders, func(getenv func(string) string) (Provider, bool) {
		token := getenv(slackEnvVarName)
		if token == "" {
			log.Println("providers: skipping Slack. if you want Slack enabled, please set a valid value for the environment variables", slackEnvVarName)
			return nil, false
		}
		return Slack(token), true
	})
}

type providerSlack struct {
	token    string
	wsURL    string
	selfID   string
	wsConnMu sync.Mutex
	wsConn   *websocket.Conn

	in  chan messages.Message
	out chan messages.Message
	err error

	mu        sync.Mutex
	usernames map[string]string
}

// Slack is the message provider meant to be used in development of rule sets.
func Slack(token string) *providerSlack {
	slack := &providerSlack{
		token:     token,
		in:        make(chan messages.Message),
		out:       make(chan messages.Message),
		usernames: make(map[string]string),
	}
	slack.handshake()
	slack.dial()
	if slack.err == nil {
		go slack.intakeLoop()
		go slack.dispatchLoop()
	}
	go slack.reconnect()
	return slack
}

func (p *providerSlack) IncomingChannel() chan messages.Message {
	return p.in
}

func (p *providerSlack) OutgoingChannel() chan messages.Message {
	return p.out
}

func (p *providerSlack) Error() error {
	return p.err
}

func (p *providerSlack) handshake() {
	log.Println("slack: connecting to HTTP API handshake interface")
	resp, err := http.Get(fmt.Sprint(urlSlackAPI, "rtm.start?no_unreads&simple_latest&token=", p.token))
	if err != nil {
		p.err = err
		return
	}
	defer resp.Body.Close()
	var data struct {
		OK   interface{} `json:"ok"`
		URL  string      `json:"url"`
		Self struct {
			ID string `json:"id"`
		} `json:"self"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		p.err = err
		return
	}

	switch v := data.OK.(type) {
	case bool:
		if !v {
			p.err = err
			return
		}
	default:
		p.err = err
		return
	}
	p.wsURL = data.URL
	p.selfID = data.Self.ID
}

func (p *providerSlack) dial() {
	log.Println("slack: dialing to HTTP WS rtm interface")
	if p.wsURL == "" {
		p.err = fmt.Errorf("could not connnect to Slack HTTP WS rtm. please, check your connection and your token (%s). error: %v", slackEnvVarName, p.err)
		return
	}
	ws, err := websocket.Dial(p.wsURL, "", urlSlackAPI)
	if err != nil {
		p.err = err
		return
	}
	p.wsConnMu.Lock()
	p.wsConn = ws
	p.wsConnMu.Unlock()
}

func (p *providerSlack) intakeLoop() {
	log.Println("slack: started message intake loop")
	for {
		var data struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
			UserID  string `json:"user"`
			Text    string `json:"text"`
		}

		p.wsConnMu.Lock()
		wsConn := p.wsConn
		p.wsConnMu.Unlock()

		if err := json.NewDecoder(wsConn).Decode(&data); err != nil {
			continue
		}

		if data.Type != "message" {
			continue
		}

		msg := messages.Message{
			Room:         data.Channel,
			FromUserID:   data.UserID,
			FromUserName: p.getUserName(data.UserID),
			Message:      data.Text,
			Direct:       strings.HasPrefix(data.Channel, "D"),
		}
		p.in <- msg
	}
}

func (p *providerSlack) getUserName(id string) string {
	p.mu.Lock()
	if name, ok := p.usernames[id]; ok {
		p.mu.Unlock()
		return name
	}
	p.mu.Unlock()

	log.Println("slack: reading username from id")
	resp, err := http.Get(fmt.Sprint(urlSlackAPI, "users.info?token=", p.token, "&user=", url.QueryEscape(id)))
	if err != nil {
		log.Println("slack: failed reading username - returning blank")
		return ""
	}
	defer resp.Body.Close()

	var data struct {
		OK   interface{} `json:"ok"`
		User struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Println("slack: failed parsing username - returning blank")
		return ""
	}

	p.mu.Lock()
	p.usernames[id] = data.User.Name
	p.mu.Unlock()

	log.Printf("slack: %s is %v", id, data.User.Name)
	return p.usernames[id]
}

func (p *providerSlack) dispatchLoop() {
	log.Println("slack: started message dispatch loop")
	for msg := range p.out {
		// TODO(ccf): find a way in that text/template does not escape username DMs.
		var finalMsg bytes.Buffer
		template.Must(template.New("tmpl").Parse(msg.Message)).Execute(&finalMsg, struct{ User string }{"<@" + msg.ToUserID + ">"})

		if strings.TrimSpace(finalMsg.String()) == "" {
			continue
		}

		data := struct {
			Type    string `json:"type"`
			User    string `json:"user"`
			Channel string `json:"channel"`
			Text    string `json:"text"`
		}{"message", p.selfID, msg.Room, html.UnescapeString(finalMsg.String())}

		// TODO(ccf): look for an idiomatic way of doing limited writers
		b, err := json.Marshal(data)
		if err != nil {
			continue
		}

		wsMsg := string(b)
		if len(wsMsg) > 16*1024 {
			continue
		}

		p.wsConnMu.Lock()
		wsConn := p.wsConn
		p.wsConnMu.Unlock()

		fmt.Fprint(wsConn, wsMsg)

		time.Sleep(1 * time.Second) // https://api.slack.com/docs/rate-limits
	}
}

func (p *providerSlack) reconnect() {
	for {
		time.Sleep(1 * time.Second)

		p.wsConnMu.Lock()
		wsConn := p.wsConn
		p.wsConnMu.Unlock()

		if wsConn == nil {
			log.Println("slack: cannot reconnect")
			break
		}

		if _, err := wsConn.Write([]byte(`{"type":"hello"}`)); err != nil {
			log.Printf("slack: reconnecting (%v)", err)
			p.handshake()
			p.dial()
		}
	}
}
