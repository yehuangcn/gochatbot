package bot // import "cirello.io/gochatbot/bot"

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"cirello.io/gochatbot/brain"
	"cirello.io/gochatbot/messages"
)

// Self encapsulates all the necessary state to have a robot running. Including
// identity (Name).
type Self struct {
	name        string
	providerIn  chan messages.Message
	providerOut chan messages.Message
	rules       []RuleParser

	brain brain.Memorizer
}

var processOnce sync.Once // protects Process

// Option type is the self-referencing method of tweaking gobot's internals.
type Option func(*Self)

// New creates a new gobot.
func New(name string, memo brain.Memorizer, opts ...Option) *Self {
	s := &Self{
		name:        name,
		brain:       memo,
		providerIn:  make(chan messages.Message),
		providerOut: make(chan messages.Message),
	}
	log.Println("bot: applying options")
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Process connects the flow of incoming messages with the ruleset, and
// dispatches the outgoing messages generated by the ruleset. Each message lives
// in its own goroutine.
func (s *Self) Process() {
	processOnce.Do(func() {
		log.Println("bot: starting main loop")
		for in := range s.providerIn {
			if strings.HasPrefix(in.Message, s.Name()+" help") {
				go func(self Self, msg messages.Message) {
					helpMsg := fmt.Sprintln("available commands:")
					for _, rule := range s.rules {
						helpMsg = fmt.Sprintln(helpMsg, rule.HelpMessage(self))
					}
					s.providerOut <- messages.Message{
						Room:       msg.Room,
						ToUserID:   msg.FromUserID,
						ToUserName: msg.FromUserName,
						Message:    helpMsg,
					}
				}(*s, in)
				continue
			}
			go func(self Self, msg messages.Message) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("panic recovered when parsing message: %#v. Panic: %v", msg, r)
					}
				}()
				for _, rule := range s.rules {
					responses := rule.ParseMessage(self, msg)
					for _, r := range responses {
						s.providerOut <- r
					}
				}
			}(*s, in)
		}
	})
}

// MemoryRead reads an arbitraty value from the robot's Brain.
func (s *Self) MemoryRead(namespace, key string) []byte {
	return s.brain.Read(namespace, key)
}

// MemorySave reads an arbitraty value from the robot's Brain.
func (s *Self) MemorySave(namespace, key string, value []byte) {
	s.brain.Save(namespace, key, value)
}

// MessageProviderOut getter for message dispatch channel
func (s *Self) MessageProviderOut() chan messages.Message {
	return s.providerOut
}

// Name returns robot's name - identity used for answering direct messages.
func (s *Self) Name() string {
	return s.name
}
