package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/xerrors"
)

func (c *wsConn) handleTryConnect(ctx context.Context) {
	defer c.ticker.Stop()
	if c.switchFile == "" {
		return
	}
	for {
		select {
		case <-c.ticker.C:
			if c.switchFile != "" {
				log.Infof("Currnet rpc connection: %s", c.curAddr)
			}
		case <-ctx.Done():
			break
		}
	}
}

// return true if switch connect to other peer
func (c *wsConn) trySwitchConnect(ctx context.Context) bool {
	if c.switchFactory == nil {
		return false
	}
	log.Info("trySwitchConnect...")
	tmpTarget, err := parseAddr(c.switchFile)
	if err != nil {
		log.Warnf("parseAddr: %s", err.Error())
		return false
	}
	if len(tmpTarget) == 0 {
		log.Warnf("try switch connect: %+v", tmpTarget)
		return false
	}
	c.switchTarget = tmpTarget
	var changeTarget []SwitchConnectInfo
	for _, tr := range c.switchTarget {
		if tr.Addr == c.curAddr {
			continue
		}
		// _, err := c.testFactory(tr)
		// if err != nil {
		// 	log.Warnf("test connection failed", "target", tr.Addr, "error", err)
		// 	continue
		// }
		changeTarget = append(changeTarget, tr)
	}
	if len(changeTarget) == 0 {
		log.Error("THERE IS NO TARGET FULLNODE TO CONNECT")
		return false
	}
	c.switchTarget = changeTarget
	target := c.switchTarget[random.Intn(len(c.switchTarget))]
	// connection dropped unexpectedly, do our best to recover it
	c.closeInFlight()
	c.closeChans()
	c.incoming = make(chan io.Reader) // listen again for responses
	func() {
		c.stopPings()
		attempts := 0
		var conn *websocket.Conn
		for conn == nil {
			time.Sleep(c.switchBackoff.next(attempts))
			var err error
			if conn, err = c.switchFactory(target); err != nil {
				log.Warnw("websocket connection switch failed", "error", err)
			}
			select {
			case <-ctx.Done():
				break
			default:
				continue
			}
			attempts++
		}
		c.writeLk.Lock()
		c.conn = conn
		c.curAddr = target.Addr
		c.curHeader = target.Header
		c.connFactory = func() (*websocket.Conn, error) {
			conn, _, err := websocket.DefaultDialer.Dial(c.curAddr, c.curHeader)
			if err != nil {
				return nil, xerrors.Errorf("connFactory dial: %s", err)
			}
			return conn, err
		}
		c.incomingErr = nil
		c.stopPings = c.setupPings()
		c.writeLk.Unlock()
		if c.conn == nil {
			return
		}
		for _, req := range c.pending {
			if err := c.sendRequest(req.req); err != nil {
				log.Errorf("sendReqest failed (Handle me): %s", err)
			}
		}
		go c.nextMessage()
	}()
	return true
}

func (c *wsConn) handleWinningWsConn(ctx context.Context) {
	c.incoming = make(chan io.Reader)
	c.inflight = map[interface{}]clientRequest{}
	c.handling = map[interface{}]context.CancelFunc{}
	c.chanHandlers = map[uint64]func(m []byte, ok bool){}
	c.pongs = make(chan struct{}, 1)

	c.registerCh = make(chan outChanReg)
	defer close(c.exiting)

	// ////

	// on close, make sure to return from all pending calls, and cancel context
	//  on all calls we handle
	defer c.closeInFlight()
	defer c.closeChans()

	// setup pings

	c.stopPings = c.setupPings()
	defer c.stopPings()

	var timeoutTimer *time.Timer
	if c.timeout != 0 {
		timeoutTimer = time.NewTimer(c.timeout)
		defer timeoutTimer.Stop()
	}

	// wait for the first message
	go c.nextMessage()
	for {
		var timeoutCh <-chan time.Time
		if timeoutTimer != nil {
			if !timeoutTimer.Stop() {
				select {
				case <-timeoutTimer.C:
				default:
				}
			}
			timeoutTimer.Reset(c.timeout)

			timeoutCh = timeoutTimer.C
		}

		start := time.Now()
		action := ""

		select {
		case r, ok := <-c.incoming:
			action = "incoming"

			err := c.incomingErr

			if ok {
				// debug util - dump all messages to stderr
				// r = io.TeeReader(r, os.Stderr)

				var frame frame
				if err = json.NewDecoder(r).Decode(&frame); err == nil {
					if frame.ID, err = normalizeID(frame.ID); err == nil {
						action = fmt.Sprintf("incoming(%s,%v)", frame.Method, frame.ID)

						c.handleFrame(ctx, frame)
						go c.nextMessage()
						break
					}
				}
			}

			if err == nil {
				return // remote closed
			}

			log.Debugw("websocket error", "error", err)
			// only client needs to reconnect
			if !c.tryReconnect(ctx) {
				if !c.trySwitchConnect(ctx) {
					return // failed to reconnect and switch connect
				}
			}
		case req := <-c.requests:
			action = fmt.Sprintf("send-request(%s,%v)", req.req.Method, req.req.ID)

			c.writeLk.Lock()
			if req.req.ID != nil { // non-notification
				if c.incomingErr != nil { // No conn?, immediate fail
					req.ready <- clientResponse{
						Jsonrpc: "2.0",
						ID:      req.req.ID,
						Error: &respError{
							Message: "handler: websocket connection closed",
							Code:    eTempWSError,
						},
					}
					c.writeLk.Unlock()
					break
				}
				c.inflight[req.req.ID] = req
			}
			c.writeLk.Unlock()
			serr := c.sendRequest(req.req)
			if serr != nil {
				log.Errorf("sendReqest failed (Handle me): %s", serr)
			}
			if req.req.ID == nil { // notification, return immediately
				resp := clientResponse{
					Jsonrpc: "2.0",
				}
				if serr != nil {
					resp.Error = &respError{
						Code:    eTempWSError,
						Message: fmt.Sprintf("sendRequest: %s", serr),
					}
				}
				req.ready <- resp
			}

		case <-c.pongs:
			action = "pong"

			c.resetReadDeadline()
		case <-timeoutCh:
			if c.pingInterval == 0 {
				// pings not running, this is perfectly normal
				continue
			}

			c.writeLk.Lock()
			if err := c.conn.Close(); err != nil {
				log.Warnw("timed-out websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			log.Errorw("Connection timeout", "remote", c.conn.RemoteAddr(), "lastAction", action)
			// The server side does not perform the reconnect operation, so need to exit
			if c.connFactory == nil {
				return
			}
			// The client performs the reconnect operation, and if it exits it cannot start a handleWsConn again, so it does not need to exit
			continue
		case <-c.stop:
			c.writeLk.Lock()
			cmsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			if err := c.conn.WriteMessage(websocket.CloseMessage, cmsg); err != nil {
				log.Warn("failed to write close message: ", err)
			}
			if err := c.conn.Close(); err != nil {
				log.Warnw("websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			return
		}

		if c.pingInterval > 0 && time.Since(start) > c.pingInterval*2 {
			log.Warnw("websocket long time no response", "lastAction", action, "time", time.Since(start))
		}
		if debugTrace {
			log.Debugw("websocket action", "lastAction", action, "time", time.Since(start))
		}
	}
}
