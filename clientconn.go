package libmqtt

import (
	"bufio"
	"net"
	"time"
)

// clientConn is the wrapper of connection to server
// tend to actual packet send and receive
type clientConn struct {
	parent     *client       // client which created this connection
	name       string        // server addr info
	conn       net.Conn      // connection to server
	connW      *bufio.Writer // make buffered connection
	logicSendC chan Packet   // logic send channel
	netRecvC   chan Packet   // received packet from server
	keepaliveC chan int      // keepalive packet
}

// start mqtt logic
func (c *clientConn) logic() {
	// start keepalive if required
	if c.parent.options.keepalive > 0 {
		go c.keepalive()
	}

	// inspect incoming packet
	for pkt := range c.netRecvC {
		switch pkt.Type() {
		case CtrlSubAck:
			p := pkt.(*SubAckPacket)
			lg.d("NET received SubAck, id =", p.PacketID)

			if originPkt, ok := c.parent.idGen.getExtra(p.PacketID); ok {
				switch originPkt.(type) {
				case *SubscribePacket:
					originSub := originPkt.(*SubscribePacket)
					N := len(p.Codes)
					for i, v := range originSub.Topics {
						if i < N {
							v.Qos = p.Codes[i]
						}
					}
					c.parent.msgC <- newSubMsg(originSub.Topics, nil)
					c.parent.idGen.free(p.PacketID)

					if err := c.parent.persist.Delete(sendKey(p.PacketID)); err != nil {
						c.parent.msgC <- newPersistMsg(err)
					}
				}
			}
		case CtrlUnSubAck:
			p := pkt.(*UnSubAckPacket)
			lg.d("NET received UnSubAck, id =", p.PacketID)

			if originPkt, ok := c.parent.idGen.getExtra(p.PacketID); ok {
				switch originPkt.(type) {
				case *UnSubPacket:
					originUnSub := originPkt.(*UnSubPacket)
					c.parent.msgC <- newUnSubMsg(originUnSub.TopicNames, nil)
					c.parent.idGen.free(p.PacketID)

					if err := c.parent.persist.Delete(sendKey(p.PacketID)); err != nil {
						c.parent.msgC <- newPersistMsg(err)
					}
				}
			}
		case CtrlPublish:
			p := pkt.(*PublishPacket)
			lg.d("NET received publish, id =", p.PacketID, "QoS =", p.Qos)
			// received server publish, send to client
			c.parent.recvC <- p

			// tend to QoS
			switch p.Qos {
			case Qos1:
				lg.d("NET send PubAck for Publish, id =", p.PacketID)
				c.send(&PubAckPacket{PacketID: p.PacketID})

				if err := c.parent.persist.Store(recvKey(p.PacketID), pkt); err != nil {
					c.parent.msgC <- newPersistMsg(err)
				}
			case Qos2:
				lg.d("NET send PubRecv for Publish, id =", p.PacketID)
				c.send(&PubRecvPacket{PacketID: p.PacketID})

				if err := c.parent.persist.Store(recvKey(p.PacketID), pkt); err != nil {
					c.parent.msgC <- newPersistMsg(err)
				}
			}
		case CtrlPubAck:
			p := pkt.(*PubAckPacket)
			lg.d("NET received PubAck, id =", p.PacketID)

			if originPkt, ok := c.parent.idGen.getExtra(p.PacketID); ok {
				switch originPkt.(type) {
				case *PublishPacket:
					originPub := originPkt.(*PublishPacket)
					if originPub.Qos == Qos1 {
						c.parent.msgC <- newPubMsg(originPub.TopicName, nil)
						c.parent.idGen.free(p.PacketID)

						if err := c.parent.persist.Delete(sendKey(p.PacketID)); err != nil {
							c.parent.msgC <- newPersistMsg(err)
						}
					}
				}
			}
		case CtrlPubRecv:
			p := pkt.(*PubRecvPacket)
			lg.d("NET received PubRec, id =", p.PacketID)

			if originPkt, ok := c.parent.idGen.getExtra(p.PacketID); ok {
				switch originPkt.(type) {
				case *PublishPacket:
					originPub := originPkt.(*PublishPacket)
					if originPub.Qos == Qos2 {
						c.send(&PubRelPacket{PacketID: p.PacketID})
						lg.d("NET send PubRel, id =", p.PacketID)
					}
				}
			}
		case CtrlPubRel:
			p := pkt.(*PubRelPacket)
			lg.d("NET send PubRel, id =", p.PacketID)

			if originPkt, ok := c.parent.idGen.getExtra(p.PacketID); ok {
				switch originPkt.(type) {
				case *PublishPacket:
					originPub := originPkt.(*PublishPacket)
					if originPub.Qos == Qos2 {
						c.send(&PubCompPacket{PacketID: p.PacketID})
						lg.d("NET send PubComp, id =", p.PacketID)

						if err := c.parent.persist.Store(recvKey(p.PacketID), pkt); err != nil {
							c.parent.msgC <- newPersistMsg(err)
						}
					}
				}
			}
		case CtrlPubComp:
			p := pkt.(*PubCompPacket)
			lg.d("NET received PubComp, id =", p.PacketID)

			if originPkt, ok := c.parent.idGen.getExtra(p.PacketID); ok {
				switch originPkt.(type) {
				case *PublishPacket:
					originPub := originPkt.(*PublishPacket)
					if originPub.Qos == Qos2 {
						c.send(&PubRelPacket{PacketID: p.PacketID})
						lg.d("NET send PubRel, id =", p.PacketID)

						c.parent.msgC <- newPubMsg(originPub.TopicName, nil)
						c.parent.idGen.free(p.PacketID)

						if err := c.parent.persist.Delete(sendKey(p.PacketID)); err != nil {
							c.parent.msgC <- newPersistMsg(err)
						}
					}
				}
			}
		default:
			lg.d("NET received packet, type =", pkt.Type())
		}
	}
}

// keepalive with server
func (c *clientConn) keepalive() {
	lg.d("NET start keepalive")

	t := time.NewTicker(c.parent.options.keepalive * 3 / 4)
	timeout := time.Duration(float64(c.parent.options.keepalive) * c.parent.options.keepaliveFactor)
	timeoutTimer := time.NewTimer(timeout)
	defer t.Stop()

	for range t.C {
		c.send(PingReqPacket)

		select {
		case _, more := <-c.keepaliveC:
			if !more {
				return
			}
			timeoutTimer.Reset(timeout)
		case <-timeoutTimer.C:
			lg.i("NET keepalive timeout")
			t.Stop()
			c.conn.Close()
			return
		}
	}

	lg.d("NET stop keepalive")
}

// close this connection
func (c *clientConn) close() {
	lg.i("NET connection to server closed, remote =", c.name)
	c.send(DisConnPacket)
}

// handle client message send
func (c *clientConn) handleClientSend() {
	for pkt := range c.parent.sendC {
		if err := EncodeOnePacket(c.parent.options.protoVersion, pkt, c.connW); err != nil {
			break
		}
		if err := c.connW.Flush(); err != nil {
			break
		}
		switch pkt.Type() {
		case CtrlPublish:
			c.parent.msgC <- newPubMsg(pkt.(*PublishPacket).TopicName, nil)
		case CtrlSubscribe:
			c.parent.msgC <- newSubMsg(pkt.(*SubscribePacket).Topics, nil)
		case CtrlUnSub:
			c.parent.msgC <- newUnSubMsg(pkt.(*UnSubPacket).TopicNames, nil)
		}
	}

}

// handle mqtt logic control packet send
func (c *clientConn) handleLogicSend() {
	for logicPkt := range c.logicSendC {
		if err := EncodeOnePacket(c.parent.options.protoVersion, logicPkt, c.connW); err != nil {
			break
		}
		if err := c.connW.Flush(); err != nil {
			break
		}
		switch logicPkt.Type() {
		case CtrlPubRel:
			if err := c.parent.persist.Store(sendKey(logicPkt.(*PubRelPacket).PacketID), logicPkt); err != nil {
				c.parent.msgC <- newPersistMsg(err)
			}
		case CtrlPubAck:
			if err := c.parent.persist.Delete(sendKey(logicPkt.(*PubAckPacket).PacketID)); err != nil {
				c.parent.msgC <- newPersistMsg(err)
			}
		case CtrlPubComp:
			if err := c.parent.persist.Delete(sendKey(logicPkt.(*PubCompPacket).PacketID)); err != nil {
				c.parent.msgC <- newPersistMsg(err)
			}
		case CtrlDisConn:
			// disconnect to server
			lg.i("disconnect to server")
			c.conn.Close()
			break
		}
	}
}

// handle all message receive
func (c *clientConn) handleRecv() {
	for {
		pkt, err := DecodeOnePacket(c.parent.options.protoVersion, c.conn)
		if err != nil {
			lg.e("NET connection broken, server =", c.name, "err =", err)
			close(c.netRecvC)
			close(c.keepaliveC)
			// TODO send proper net error to net handler
			if err != ErrDecodeBadPacket {
				// c.parent.msgC <- newNetMsg(c.name, err)
			}
			break
		}

		if pkt == PingRespPacket {
			lg.d("NET received keepalive message")
			c.keepaliveC <- 1
		} else {
			c.netRecvC <- pkt
		}
	}
}

// send mqtt logic packet
func (c *clientConn) send(pkt Packet) {
	c.logicSendC <- pkt
}
