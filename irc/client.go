// Package irc provides a specialised IRC client designed for gathering
// statistics only.
package irc

import (
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/sorcix/irc.v2"
)

var (
	numberRE   = regexp.MustCompile(`\d+`)
	servicesRE = regexp.MustCompile(`^- Registered (\S+?): (\d+)`)
)

type Client struct {
	Server    string
	options   *Options
	connected bool
	inCh      chan *irc.Message
	statsCh   chan *StatsRequest
}

func NewClient(options Options) *Client {
	return &Client{
		options: &options,
		statsCh: make(chan *StatsRequest),
	}
}

// Start manages a connection to the server. It does not return (run it in a goroutine).
func (c *Client) Start() {
	for {
		c.doConnection()
		// TODO: backoff? Not really an issue when talking to localhost...
		time.Sleep(5 * time.Second)
	}
}

func (c *Client) Stats(s StatsRequest) StatsResponse {
	s.response = make(chan StatsResponse)
	c.statsCh <- &s
	return <-s.response
}

func (c *Client) doConnection() {
	doneCh := make(chan bool)
	messageCh := make(chan *irc.Message)
	inCh := make(chan *irc.Message)
	go c.handleConnection(messageCh, inCh, doneCh)
	c.inCh = inCh

	statsReq := []*StatsRequest{}
	statsRes := StatsResponse{
		Servers: make(map[string]*ServerStats),
		Nicks:   make(map[string]bool),
	}
	inProgress := false
	var timeout *time.Time

	doneRes := func() {
		doneCount := 0
		for _, r := range statsRes.Servers {
			if r.done {
				doneCount++
			}
		}
		if doneCount != len(statsRes.Servers) && !statsRes.Timeout {
			return
		}
		if len(statsReq) > 0 && len(statsReq[0].Nicks) > 0 && (len(statsRes.Nicks) == 0 && !statsRes.Timeout) {
			return
		}
		for _, req := range statsReq {
			req.response <- statsRes
		}
		statsReq = []*StatsRequest{}
		statsRes = StatsResponse{
			Servers: make(map[string]*ServerStats),
			Nicks:   make(map[string]bool),
		}
		inProgress = false
		timeout = nil
	}

	for {
		select {
		case m, ok := <-messageCh:
			if !ok {
				continue
			}
			switch m.Command {
			case irc.RPL_LINKS:
				if inProgress {
					server := m.Params[1]
					skip := false
					for _, ignore := range statsReq[0].IgnoreServers {
						if ignore == server {
							skip = true
						}
					}
					if skip {
						break
					}
					statsRes.Servers[server] = &ServerStats{}
					s := statsRes.Servers[server]
					s.Up = false
					s.Commands = make(map[string]*CommandStats)
					// This assumes the server includes a distance in the /LINKS output, a
					// common extension.
					// TODO: We could just make /LINKS into a tree, would support ~all
					// IRCds that way. (PRs welcome.)
					desc := strings.Split(m.Params[3], " ")
					distance, err := strconv.Atoi(desc[0])
					if err == nil {
						s.Distance = distance
					} else {
						log.Printf("failed to parse distance from: %v", m)
					}
					if !statsReq[0].Local || server == c.Server {
						s.RequestTime = time.Now()
						inCh <- &irc.Message{
							Command: irc.LUSERS,
							Params:  []string{server, server},
						}
						//						inCh <- &irc.Message{
						//							Command: irc.STATS,
						//							Params:  []string{"m", server},
						//						}
					} else {
						// We're not going to query it, but we saw it there in links, best we can do
						s.done = true
					}
				}
			case irc.RPL_LUSERCLIENT:
				if inProgress {
					// Time the first expected reply line to /LUSERS
					s, ok := statsRes.Servers[m.Prefix.Name]
					if ok {
						s.ResponseTime = time.Now()
						s.Up = true
					}
				}
			case irc.RPL_LUSERCHANNELS:
				if inProgress {
					if m.Prefix.Name == c.Server {
						channels, err := strconv.Atoi(m.Params[1])
						if err == nil {
							statsRes.Channels = channels
						} else {
							log.Printf("failed to parse channel count from: %v", m)
						}
					}
				}
			case irc.RPL_LUSERME:
				// Note we could also look at the Hybrid specific 265 (RPL_LOCALUSERS,
				// https://github.com/grawity/irc-docs/blob/master/alien.net.au/irc2numerics.def#L845)
				// Would avoid the text parsing. But this should work on any RFC1459 IRCd.
				if inProgress {
					s, ok := statsRes.Servers[m.Prefix.Name]
					if ok {
						// "I have X clients and Y servers"
						x := numberRE.FindString(m.Params[1])
						users, err := strconv.Atoi(x)
						if err == nil {
							s.Users = users
						} else {
							log.Printf("failed to parse user count from: %v", m)
						}
					}
				}
			case irc.RPL_STATSCOMMANDS:
				if inProgress {
					s, ok := statsRes.Servers[m.Prefix.Name]
					if ok {
						count, cerr := strconv.Atoi(m.Params[2])
						rcount, rerr := strconv.Atoi(m.Params[2])
						bytes, berr := strconv.Atoi(m.Params[2])
						if cerr == nil && rerr == nil && berr == nil {
							// s.Commands[m.Params[1]].Clients = count
							s.Commands[m.Params[1]] = &CommandStats{}
							s.Commands[m.Params[1]].Clients = count - rcount
							s.Commands[m.Params[1]].Server = rcount
							s.Commands[m.Params[1]].Bytes = bytes
						}
					}
				}

			case irc.RPL_MOTD:
				if inProgress {
					s, ok := statsRes.Servers[m.Prefix.Name]
					_ = s
					if ok {
						srm := servicesRE.FindSubmatch([]byte(m.Params[1]))
						if srm != nil {
							fmt.Println(`OK`)
							if string(srm[1]) == `users` {
								susers, serr := strconv.Atoi(string(srm[2]))
								if serr != nil {
									fmt.Println(susers)
								}
							}
							if string(srm[1]) == `channels` {
								schannels, cerr := strconv.Atoi(string(srm[2]))
								if cerr != nil {
									fmt.Println(schannels)
								}
							}
						}
					}
				}

			case irc.RPL_ENDOFSTATS:
				if inProgress {
					s, ok := statsRes.Servers[m.Prefix.Name]
					if ok {
						s.done = true
						doneRes()
					}
				}
			case irc.RPL_ISON:
				if inProgress {
					ison := strings.Split(m.Params[1], " ")
					on := map[string]bool{}
					for _, nick := range ison {
						on[strings.ToLower(nick)] = true
					}
					for _, nick := range statsReq[0].Nicks {
						statsRes.Nicks[nick] = on[strings.ToLower(nick)]
					}
					doneRes()
				}
			case irc.ERR_NOSUCHSERVER:
				if inProgress {
					s, ok := statsRes.Servers[m.Params[1]]
					if ok {
						s.done = true
						doneRes()
					}
				}
			}
		case req := <-c.statsCh:
			// We just combine all requests, could be confusing with a high timeout...
			// TODO: This means requests can't have different parameters.
			statsReq = append(statsReq, req)
			if !c.connected {
				statsRes.Timeout = true
				doneRes()
			} else if !inProgress {
				// Links response triggers the rest of the commands, above.
				inCh <- &irc.Message{
					Command: irc.LINKS,
				}
				// Get stats from services
				inCh <- &irc.Message{
					Command: irc.MOTD,
					Params:  []string{"OperServ"},
				}
				if len(req.Nicks) > 0 {
					inCh <- &irc.Message{
						Command: irc.ISON,
						Params:  req.Nicks,
					}
				}
				inProgress = true
				t := time.Now().Add(req.Timeout)
				timeout = &t
			}
		case <-time.After(1 * time.Second):
			if timeout != nil && time.Now().After(*timeout) {
				statsRes.Timeout = true
				doneRes()
			}
		case <-doneCh:
			statsRes.Timeout = true
			doneRes()
			return
		}
	}
}

func (c *Client) handleConnection(messageCh, inCh chan *irc.Message, doneCh chan bool) {
	defer func() {
		c.connected = false
		close(messageCh)
		close(inCh)
		doneCh <- true
	}()
	conn, err := irc.Dial(c.options.Server)
	if err != nil {
		log.Printf("connect failed: %v", err)
		return
	}
	defer conn.Close()

	go func() {
		for m := range inCh {
			if m.Command == irc.PASS || m.Command == irc.OPER {
				log.Print("> ", m.Command, " hunter2")
			} else {
				log.Print("> ", m)
			}
			conn.Encode(m)
		}
	}()

	if c.options.Password != "" {
		inCh <- &irc.Message{
			Command: irc.PASS,
			Params:  []string{c.options.Password},
		}
	}
	inCh <- &irc.Message{
		Command: irc.USER,
		Params:  []string{c.options.Nick, "x", "x", "Prometheus IRC exporter, http://d.cx/prom-ircd-exp"},
	}
	inCh <- &irc.Message{
		Command: irc.NICK,
		Params:  []string{c.options.Nick},
	}

	for {
		m, err := conn.Decode()
		if err != nil {
			log.Printf("read failed: %v", err)
			break
		}

		log.Print("< ", m)

		switch m.Command {
		case irc.RPL_WELCOME:
			c.connected = true
			c.Server = m.Prefix.Name
			log.Printf("- Connected to %v", c.Server)
			if len(c.options.OperUser) > 0 {
				inCh <- &irc.Message{
					Command: irc.OPER,
					Params:  []string{c.options.OperUser, c.options.OperPassword},
				}
			}
		case irc.ERR_NICKNAMEINUSE:
			inCh <- &irc.Message{
				Command: irc.NICK,
				Params:  []string{c.options.Nick + fmt.Sprintf("%03d", rand.Intn(1000))},
			}
		case irc.PING:
			inCh <- &irc.Message{
				Command: irc.PONG,
				Params:  m.Params,
			}
		case irc.ERROR, irc.QUIT:
			log.Print("! ", m)
		}
		messageCh <- m
	}
}
