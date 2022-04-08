package wrtcconn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	websocketapi "github.com/pojntfx/webrtcfd/internal/api/websocket"
	"github.com/pojntfx/webrtcfd/internal/encryption"
)

var (
	ErrInvalidTURNServerAddr  = errors.New("invalid TURN server address")
	ErrMissingTURNCredentials = errors.New("missing TURN server credentials")
)

type peer struct {
	conn       *webrtc.PeerConnection
	candidates chan webrtc.ICECandidateInit
	channels   map[string]*webrtc.DataChannel
	iid        string
}

type Peer struct {
	PeerID    string
	ChannelID string
	Conn      io.ReadWriteCloser
}

type AdapterConfig struct {
	Timeout          time.Duration
	Verbose          bool
	ID               string
	PrimaryChannelID string
}

type Adapter struct {
	signaler string
	key      string
	ice      []string
	config   *AdapterConfig
	ctx      context.Context

	cancel context.CancelFunc
	done   bool
	lines  chan []byte

	peerChan chan *Peer
	peers    map[string]*peer
	peerLock sync.Mutex
}

func NewAdapter(
	signaler string,
	key string,
	ice []string,
	config *AdapterConfig,
	ctx context.Context,
) *Adapter {
	ictx, cancel := context.WithCancel(ctx)

	if config == nil {
		config = &AdapterConfig{
			Timeout:          time.Second * 10,
			Verbose:          false,
			ID:               "",
			PrimaryChannelID: "",
		}
	}

	if strings.TrimSpace(config.PrimaryChannelID) == "" {
		config.PrimaryChannelID = "primary"
	}

	return &Adapter{
		signaler: signaler,
		key:      key,
		ice:      ice,
		config:   config,
		ctx:      ictx,

		cancel:   cancel,
		lines:    make(chan []byte),
		peerChan: make(chan *Peer),
		peers:    map[string]*peer{},
	}
}

func (a *Adapter) Open() (chan string, error) {
	ids := make(chan string)

	u, err := url.Parse(a.signaler)
	if err != nil {
		return ids, err
	}

	community := u.Query().Get("community")

	iceServers := []webrtc.ICEServer{}

	for _, ice := range a.ice {
		// Skip empty server configs
		if strings.TrimSpace(ice) == "" {
			continue
		}

		if strings.Contains(ice, "stun:") {
			iceServers = append(iceServers, webrtc.ICEServer{
				URLs: []string{ice},
			})
		} else {
			addrParts := strings.Split(ice, "@")
			if len(addrParts) < 2 {
				return ids, ErrInvalidTURNServerAddr
			}

			authParts := strings.Split(addrParts[0], ":")
			if len(addrParts) < 2 {
				return ids, ErrMissingTURNCredentials
			}

			iceServers = append(iceServers, webrtc.ICEServer{
				URLs:           []string{addrParts[1]},
				Username:       authParts[0],
				Credential:     authParts[1],
				CredentialType: webrtc.ICECredentialTypePassword,
			})
		}
	}

	go func() {
		for {
			if a.done {
				return
			}

			func() {
				defer func() {
					a.peerLock.Lock()
					a.peers = map[string]*peer{}
					a.peerLock.Unlock()

					if err := recover(); err != nil {
						if a.config.Verbose {
							log.Println("closed connection to signaler with address", u.String()+":", err, "(wrong username or password?)")
						}
					}

					if a.config.Verbose {
						log.Println("Reconnecting to signaler with address", u.String(), "in", a.config.Timeout)
					}

					time.Sleep(a.config.Timeout)
				}()

				ctx, cancel := context.WithTimeout(a.ctx, a.config.Timeout)
				defer cancel()

				conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
				if err != nil {
					panic(err)
				}

				defer func() {
					if a.config.Verbose {
						log.Println("Disconnected from signaler with address", u.String())
					}

					if err := conn.Close(); err != nil {
						panic(err)
					}

					a.peerLock.Lock()
					defer a.peerLock.Unlock()

					for _, peer := range a.peers {
						for _, channel := range peer.channels {
							if err := channel.Close(); err != nil {
								panic(err)
							}
						}

						if err := peer.conn.Close(); err != nil {
							panic(err)
						}

						close(peer.candidates)
					}
				}()

				if err := conn.SetReadDeadline(time.Now().Add(a.config.Timeout)); err != nil {
					panic(err)
				}
				conn.SetPongHandler(func(string) error {
					return conn.SetReadDeadline(time.Now().Add(a.config.Timeout))
				})

				if a.config.Verbose {
					log.Println("Connected to signaler with address", u.String())
				}

				inputs := make(chan []byte)
				errs := make(chan error)
				go func() {
					defer func() {
						close(inputs)
						close(errs)
					}()

					for {
						_, p, err := conn.ReadMessage()
						if err != nil {
							errs <- err

							return
						}

						inputs <- p
					}
				}()

				id := a.config.ID
				if strings.TrimSpace(id) == "" {
					id = uuid.New().String()
				}

				ids <- id

				go func() {
					p, err := json.Marshal(websocketapi.NewIntroduction(id))
					if err != nil {
						errs <- err

						return
					}

					a.lines <- p

					if a.config.Verbose {
						log.Println("Introduced to signaler with address", u.String(), "and ID", id)
					}
				}()

				pings := time.NewTicker(a.config.Timeout / 2)
				defer pings.Stop()

				for {
					select {
					case err := <-errs:
						panic(err)
					case input := <-inputs:
						input, err = encryption.Decrypt(input, []byte(a.key))
						if err != nil {
							if a.config.Verbose {
								log.Println("Could not decrypt message with length", len(input), "for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
							}

							continue
						}

						if a.config.Verbose {
							log.Println("Received message with length", len(input), "from signaler with address", conn.RemoteAddr(), "in community", community)
						}

						var message websocketapi.Message
						if err := json.Unmarshal(input, &message); err != nil {
							if a.config.Verbose {
								log.Println("Could not unmarshal message for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
							}

							continue
						}

						switch message.Type {
						case websocketapi.TypeIntroduction:
							var introduction websocketapi.Introduction
							if err := json.Unmarshal(input, &introduction); err != nil {
								if a.config.Verbose {
									log.Println("Could not unmarshal introduction for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
								}

								continue
							}

							if a.config.Verbose {
								log.Println("Received introduction", introduction, "from signaler with address", conn.RemoteAddr(), "in community", community)
							}

							iid := uuid.NewString()

							c, err := webrtc.NewPeerConnection(webrtc.Configuration{
								ICEServers: iceServers,
							})
							if err != nil {
								panic(err)
							}

							c.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
								if pcs == webrtc.PeerConnectionStateDisconnected {
									if a.config.Verbose {
										log.Println("Disconnected from peer", introduction.From)
									}

									a.peerLock.Lock()
									defer a.peerLock.Unlock()

									c, ok := a.peers[introduction.From]

									if !ok {
										if a.config.Verbose {
											log.Println("Could not find connection for peer", introduction.From, ", skipping")
										}

										return
									}

									if c.iid != iid {
										if a.config.Verbose {
											log.Println("Peer", introduction.From, ", already rejoined, not disconnecting")
										}

										return
									}

									for _, channel := range c.channels {
										if err := channel.Close(); err != nil {
											panic(err)
										}
									}

									if err := c.conn.Close(); err != nil {
										panic(err)
									}

									close(c.candidates)

									delete(a.peers, introduction.From)
								}
							})

							c.OnICECandidate(func(i *webrtc.ICECandidate) {
								if i != nil {
									if a.config.Verbose {
										log.Println("Created ICE candidate", i, "for signaler with address", conn.RemoteAddr(), "in community", community)
									}

									p, err := json.Marshal(websocketapi.NewCandidate(id, introduction.From, []byte(i.ToJSON().Candidate)))
									if err != nil {
										panic(err)
									}

									go func() {
										a.lines <- p

										if a.config.Verbose {
											log.Println("Sent candidate to signaler with address", u.String(), "and ID", id, "to client", introduction.From)
										}
									}()
								}
							})

							c.OnDataChannel(func(dc *webrtc.DataChannel) {
								dc.OnOpen(func() {
									if a.config.Verbose {
										log.Println("Connected to channel", dc.Label(), "with peer", introduction.From)
									}

									a.peerLock.Lock()
									a.peers[introduction.From].channels[dc.Label()] = dc
									a.peerChan <- &Peer{introduction.From, dc.Label(), newDataChannelReadWriteCloser(dc)}
									a.peerLock.Unlock()
								})

								dc.OnClose(func() {
									if a.config.Verbose {
										log.Println("Disconnected from channel", dc.Label(), "with peer", introduction.From)
									}

									a.peerLock.Lock()
									defer a.peerLock.Unlock()
									channel, ok := a.peers[introduction.From].channels[dc.Label()]
									if !ok {
										if a.config.Verbose {
											log.Println("Could not find channel", dc.Label(), "for peer", introduction.From, ", skipping")

										}

										return
									}

									if err := channel.Close(); err != nil {
										panic(err)
									}

									delete(a.peers[introduction.From].channels, dc.Label())
								})
							})

							dc, err := c.CreateDataChannel(a.config.PrimaryChannelID, nil)
							if err != nil {
								panic(err)
							}

							if a.config.Verbose {
								log.Println("Created data channel using signaler with address", conn.RemoteAddr(), "in community", community)
							}

							pr := &peer{c, make(chan webrtc.ICECandidateInit), map[string]*webrtc.DataChannel{
								dc.Label(): dc,
							}, iid}

							dc.OnOpen(func() {
								if a.config.Verbose {
									log.Println("Connected to channel", dc.Label(), "with peer", introduction.From)
								}

								a.peerLock.Lock()
								a.peers[introduction.From].channels[dc.Label()] = dc
								a.peerChan <- &Peer{introduction.From, dc.Label(), newDataChannelReadWriteCloser(dc)}
								a.peerLock.Unlock()
							})

							dc.OnClose(func() {
								if a.config.Verbose {
									log.Println("Disconnected from channel", dc.Label(), "with peer", introduction.From)
								}

								a.peerLock.Lock()
								defer a.peerLock.Unlock()
								channel, ok := a.peers[introduction.From].channels[dc.Label()]
								if !ok {
									if a.config.Verbose {
										log.Println("Could not find channel", dc.Label(), "for peer", introduction.From, ", skipping")

									}

									return
								}

								if err := channel.Close(); err != nil {
									panic(err)
								}

								delete(a.peers[introduction.From].channels, dc.Label())
							})

							o, err := c.CreateOffer(nil)
							if err != nil {
								panic(err)
							}

							if err := c.SetLocalDescription(o); err != nil {
								panic(err)
							}

							oj, err := json.Marshal(o)
							if err != nil {
								panic(err)
							}

							p, err := json.Marshal(websocketapi.NewOffer(id, introduction.From, oj))
							if err != nil {
								panic(err)
							}

							a.peerLock.Lock()
							old, ok := a.peers[introduction.From]
							if ok {
								// Disconnect the old peer
								if a.config.Verbose {
									log.Println("Disconnected from peer", introduction.From)
								}

								for _, channel := range old.channels {
									if err := channel.Close(); err != nil {
										panic(err)
									}
								}

								if err := old.conn.Close(); err != nil {
									panic(err)
								}

								close(old.candidates)
							}
							a.peers[introduction.From] = pr
							a.peerLock.Unlock()

							go func() {
								a.lines <- p

								if a.config.Verbose {
									log.Println("Sent offer to signaler with address", u.String(), "and ID", id, "to client", introduction.From)
								}
							}()
						case websocketapi.TypeOffer:
							var offer websocketapi.Exchange
							if err := json.Unmarshal(input, &offer); err != nil {
								if a.config.Verbose {
									log.Println("Could not unmarshal offer for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
								}

								continue
							}

							if a.config.Verbose {
								log.Println("Received offer", offer, "from signaler with address", conn.RemoteAddr(), "in community", community)
							}

							if offer.To != id {
								if a.config.Verbose {
									log.Println("Discarding offer", offer, "from signaler with address", conn.RemoteAddr(), "in community", community, "because it is not intended for this client")
								}

								continue
							}

							iid := uuid.NewString()

							c, err := webrtc.NewPeerConnection(webrtc.Configuration{
								ICEServers: iceServers,
							})
							if err != nil {
								panic(err)
							}

							c.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
								if pcs == webrtc.PeerConnectionStateDisconnected {
									if a.config.Verbose {
										log.Println("Disconnected from peer", offer.From)
									}

									a.peerLock.Lock()
									defer a.peerLock.Unlock()

									c, ok := a.peers[offer.From]
									if !ok {
										if a.config.Verbose {
											log.Println("Could not find connection for peer", offer.From, ", skipping")
										}

										return
									}

									if c.iid != iid {
										if a.config.Verbose {
											log.Println("Peer", offer.From, ", already rejoined, not disconnecting")
										}

										return
									}

									if err := c.conn.Close(); err != nil {
										panic(err)
									}

									if err := c.conn.Close(); err != nil {
										panic(err)
									}

									close(c.candidates)

									delete(a.peers, offer.From)
								}
							})

							c.OnICECandidate(func(i *webrtc.ICECandidate) {
								if i != nil {
									if a.config.Verbose {
										log.Println("Created ICE candidate", i, "for signaler with address", conn.RemoteAddr(), "in community", community)
									}

									p, err := json.Marshal(websocketapi.NewCandidate(id, offer.From, []byte(i.ToJSON().Candidate)))
									if err != nil {
										panic(err)
									}

									go func() {
										a.lines <- p

										if a.config.Verbose {
											log.Println("Sent candidate to signaler with address", u.String(), "and ID", id, "to client", offer.From)
										}
									}()
								}
							})

							c.OnDataChannel(func(dc *webrtc.DataChannel) {
								dc.OnOpen(func() {
									if a.config.Verbose {
										log.Println("Connected to channel", dc.Label(), "with peer", offer.From)
									}

									a.peerLock.Lock()
									a.peers[offer.From].channels[dc.Label()] = dc
									a.peerChan <- &Peer{offer.From, dc.Label(), newDataChannelReadWriteCloser(dc)}
									a.peerLock.Unlock()
								})

								dc.OnClose(func() {
									if a.config.Verbose {
										log.Println("Disconnected from channel", dc.Label(), "with peer", offer.From)
									}

									a.peerLock.Lock()
									defer a.peerLock.Unlock()
									channel, ok := a.peers[offer.From].channels[dc.Label()]
									if !ok {
										if a.config.Verbose {
											log.Println("Could not find channel", dc.Label(), "for peer", offer.From, ", skipping")

										}

										return
									}

									if err := channel.Close(); err != nil {
										panic(err)
									}

									delete(a.peers[offer.From].channels, dc.Label())
								})
							})

							var sdp webrtc.SessionDescription
							if err := json.Unmarshal(offer.Payload, &sdp); err != nil {
								if a.config.Verbose {
									log.Println("Could not unmarshal SDP for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
								}

								continue
							}

							if err := c.SetRemoteDescription(sdp); err != nil {
								panic(err)
							}

							ans, err := c.CreateAnswer(nil)
							if err != nil {
								panic(err)
							}

							if err := c.SetLocalDescription(ans); err != nil {
								panic(err)
							}

							aj, err := json.Marshal(ans)
							if err != nil {
								panic(err)
							}

							p, err := json.Marshal(websocketapi.NewAnswer(id, offer.From, aj))
							if err != nil {
								panic(err)
							}

							a.peerLock.Lock()

							candidates := make(chan webrtc.ICECandidateInit)
							a.peers[offer.From] = &peer{c, candidates, map[string]*webrtc.DataChannel{}, iid}

							a.peerLock.Unlock()

							go func() {
								for candidate := range candidates {
									if err := c.AddICECandidate(candidate); err != nil {
										errs <- err

										return
									}

									if a.config.Verbose {
										log.Println("Added ICE candidate from signaler with address", u.String(), "and ID", id, "from client", offer.From)
									}
								}
							}()

							go func() {
								a.lines <- p

								if a.config.Verbose {
									log.Println("Sent answer to signaler with address", u.String(), "and ID", id, "to client", offer.From)
								}
							}()
						case websocketapi.TypeCandidate:
							var candidate websocketapi.Exchange
							if err := json.Unmarshal(input, &candidate); err != nil {
								if a.config.Verbose {
									log.Println("Could not unmarshal candidate for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
								}

								continue
							}

							if a.config.Verbose {
								log.Println("Received candidate", candidate, "from signaler with address", conn.RemoteAddr(), "in community", community)
							}

							if candidate.To != id {
								if a.config.Verbose {
									log.Println("Discarding candidate", candidate, "from signaler with address", conn.RemoteAddr(), "in community", community, "because it is not intended for this client")
								}

								continue
							}

							a.peerLock.Lock()
							c, ok := a.peers[candidate.From]

							if !ok {
								if a.config.Verbose {
									log.Println("Could not find connection for peer", candidate.From, ", skipping")
								}

								a.peerLock.Unlock()

								continue
							}

							go func() {
								defer func() {
									if err := recover(); err != nil {
										if a.config.Verbose {
											log.Println("Gathering candidates has stopped, skipping candidate")
										}
									}
								}()

								c.candidates <- webrtc.ICECandidateInit{Candidate: string(candidate.Payload)}
							}()

							a.peerLock.Unlock()
						case websocketapi.TypeAnswer:
							var answer websocketapi.Exchange
							if err := json.Unmarshal(input, &answer); err != nil {
								if a.config.Verbose {
									log.Println("Could not unmarshal answer for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
								}

								continue
							}

							if a.config.Verbose {
								log.Println("Received answer", answer, "from signaler with address", conn.RemoteAddr(), "in community", community)
							}

							if answer.To != id {
								if a.config.Verbose {
									log.Println("Discarding answer", answer, "from signaler with address", conn.RemoteAddr(), "in community", community, "because it is not intended for this client")
								}

								continue
							}

							a.peerLock.Lock()
							c, ok := a.peers[answer.From]
							a.peerLock.Unlock()

							if !ok {
								if a.config.Verbose {
									log.Println("Could not find connection for peer", answer.From, ", skipping")
								}

								continue
							}

							var sdp webrtc.SessionDescription
							if err := json.Unmarshal(answer.Payload, &sdp); err != nil {
								if a.config.Verbose {
									log.Println("Could not unmarshal SDP for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
								}

								continue
							}

							if err := c.conn.SetRemoteDescription(sdp); err != nil {
								panic(err)
							}

							go func() {
								for candidate := range c.candidates {
									if err := c.conn.AddICECandidate(candidate); err != nil {
										errs <- err

										return
									}

									if a.config.Verbose {
										log.Println("Added ICE candidate from signaler with address", u.String(), "and ID", id, "from client", answer.From)
									}
								}
							}()

							if a.config.Verbose {
								log.Println("Added answer from signaler with address", u.String(), "and ID", id, "from client", answer.From)
							}
						default:
							if a.config.Verbose {
								log.Println("Got message with unknown type", message.Type, "for signaler with address", conn.RemoteAddr(), "in community", community+", skipping")
							}

							continue
						}
					case line := <-a.lines:
						line, err = encryption.Encrypt(line, []byte(a.key))
						if err != nil {
							panic(err)
						}

						if a.config.Verbose {
							log.Println("Sending message with length", len(line), "to signaler with address", conn.RemoteAddr(), "in community", community)
						}

						if err := conn.WriteMessage(websocket.TextMessage, line); err != nil {
							panic(err)
						}

						if err := conn.SetWriteDeadline(time.Now().Add(a.config.Timeout)); err != nil {
							panic(err)
						}
					case <-pings.C:
						if a.config.Verbose {
							log.Println("Sending ping to signaler with address", conn.RemoteAddr(), "in community", community)
						}

						if err := conn.SetWriteDeadline(time.Now().Add(a.config.Timeout)); err != nil {
							panic(err)
						}

						if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
							panic(err)
						}
					}
				}
			}()
		}
	}()

	return ids, nil
}

func (a *Adapter) Close() error {
	a.done = true

	a.cancel()

	close(a.lines)

	return nil
}

func (a *Adapter) Accept() chan *Peer {
	return a.peerChan
}

type message struct {
	data []byte
	err  error
}

type dataChannelReadWriteCloser struct {
	dc   *webrtc.DataChannel
	msgs chan message
}

func newDataChannelReadWriteCloser(
	dc *webrtc.DataChannel,
) *dataChannelReadWriteCloser {
	d := &dataChannelReadWriteCloser{dc, make(chan message)}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		d.msgs <- message{msg.Data, nil}
	})

	dc.OnClose(func() {
		d.msgs <- message{[]byte{}, io.EOF}
	})

	return d
}

func (d *dataChannelReadWriteCloser) Read(p []byte) (n int, err error) {
	msg := <-d.msgs

	if msg.err != nil {
		return -1, msg.err
	}

	return copy(p, msg.data), nil
}
func (d *dataChannelReadWriteCloser) Write(p []byte) (n int, err error) {
	if err := d.dc.Send(p); err != nil {
		return -1, err
	}

	return len(p), nil
}
func (d *dataChannelReadWriteCloser) Close() error {
	return d.dc.Close()
}
