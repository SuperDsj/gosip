// SIP Transport Layer.  Responsible for serializing messages to/from
// your network.

package sip

import (
	"bytes"
	"errors"
	"flag"
	"github.com/jart/gosip/util"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	tracing          = flag.Bool("tracing", false, "Enable SIP message tracing")
	timestampTagging = flag.Bool("timestampTagging", false, "Add microsecond timestamps to Via tags")
)

// Transport sends and receives SIP messages over UDP with stateless routing.
type Transport struct {

	// Thing returned by ListenPacket
	Sock *net.UDPConn

	// When you send an outbound request (not a response) you have to set the via
	// tag: ``msg.Via = tp.Via.Copy().Branch().SetNext(msg.Via)``. The details of
	// the branch parameter... are tricky.
	Via *Via

	// Returns a linked list of all canonical names and/or IP addresses that may
	// be used to contact *this specific* transport.
	Contact *Addr

	// Reusable memory for serialization
	buf []byte
}

// Creates a new stateless network mechanism for transmitting and receiving SIP
// signalling messages.
//
// contact is a SIP address, e.g. "<sip:1.2.3.4>", that tells how to bind
// sockets. If contact.Uri.Port is 0, then a port will be selected randomly.
// This value is also used for contact headers which tell other user-agents
// where to send responses and hence should only contain an IP or canonical
// address.
func NewTransport(contact *Addr) (tp *Transport, err error) {
	saddr := util.HostPortToString(contact.Uri.Host, contact.Uri.Port)
	sock, err := net.ListenPacket("udp", saddr)
	if err != nil {
		return nil, err
	}
	addr := sock.LocalAddr().(*net.UDPAddr)
	contact = contact.Copy()
	contact.Uri.Port = uint16(addr.Port)
	contact.Uri.Params["transport"] = addr.Network()
	return &Transport{
		Sock:    sock.(*net.UDPConn),
		Contact: contact,
		Via: &Via{
			Host: contact.Uri.Host,
			Port: contact.Uri.Port,
		},
	}, nil
}

// Sends a SIP message.
func (tp *Transport) Send(msg *Msg) error {
	msg, saddr, err := tp.route(msg)
	if err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("ip", saddr)
	if err != nil {
		return err
	}
	if msg.MaxForwards > 0 {
		msg.MaxForwards--
	}
	ts := time.Now()
	addTimestamp(msg, ts)
	var b bytes.Buffer
	msg.Append(&b)
	if *tracing {
		tp.trace("send", b.String(), addr, ts)
	}
	_, err = tp.Sock.WriteTo(b.Bytes(), addr)
	if err != nil {
		return err
	}
	return nil
}

// Receives a SIP message. The received address is injected into the first Via
// header as the "received" param. The Error field of msg should be checked. If
// msg.Status is Status8xxNetworkError, it means the underlying socket died. If
// you set a deadline, you should check: util.IsTimeout(msg.Error).
//
// Warning: Must only be called by one goroutine.
func (tp *Transport) Recv() *Msg {
	if tp.buf == nil {
		tp.buf = make([]byte, 2048)
	}
	amt, addr, err := tp.Sock.ReadFromUDP(tp.buf)
	if err != nil {
		return &Msg{Status: Status8xxNetworkError, Error: err}
	}
	ts := time.Now()
	packet := string(tp.buf[0:amt])
	if *tracing {
		tp.trace("recv", packet, addr, ts)
	}
	// Validation: http://tools.ietf.org/html/rfc3261#section-16.3
	msg := ParseMsg(packet)
	if msg.Error != nil {
		return msg
	}
	addReceived(msg, addr)
	addTimestamp(msg, ts)
	if !tp.sanityCheck(msg) {
		return msg
	}
	tp.preprocess(msg)
	return msg
}

func (tp *Transport) trace(dir, pkt string, addr *net.UDPAddr, t time.Time) {
	size := len(pkt)
	bar := strings.Repeat("-", 72)
	suffix := "\n  "
	if pkt[len(pkt)-1] == '\n' {
		suffix = ""
	}
	log.Printf(
		"%s %d bytes to %s/%s at %s\n"+
			"%s\n"+
			"%s%s"+
			"%s\n",
		dir, size, addr.Network(), addr.String(),
		t.Format(time.RFC3339Nano),
		bar,
		strings.Replace(pkt, "\n", "\n  ", -1), suffix,
		bar)
}

// Checks if message is acceptable, otherwise sets msg.Error and returns false.
func (tp *Transport) sanityCheck(msg *Msg) bool {
	if msg.MaxForwards <= 0 {
		go tp.Send(NewResponse(msg, 483))
		msg.Error = errors.New("Froot loop detected")
	}
	if msg.IsResponse {
		if msg.Status >= 700 {
			go tp.Send(NewResponse(msg, 400))
			msg.Error = errors.New("Crazy status number")
		}
	} else {
		if msg.CSeqMethod == "" || msg.CSeqMethod != msg.Method {
			go tp.Send(NewResponse(msg, 400))
			msg.Error = errors.New("Bad CSeq")
		}
	}
	return msg.Error == nil
}

// Perform some ingress message mangling.
func (tp *Transport) preprocess(msg *Msg) {
	if tp.Contact.Compare(msg.Route) {
		log.Printf("Removing our route header: %s", msg.Route)
		msg.Route = msg.Route.Next
	}
	if _, ok := msg.Request.Params["lr"]; ok && msg.Route != nil && tp.Contact.Uri.Compare(msg.Request) {
		// RFC3261 16.4 Route Information Preprocessing
		// RFC3261 16.12.1.2: Traversing a Strict-Routing Proxy
		var oldReq, newReq *URI
		if msg.Route.Next == nil {
			oldReq, newReq = msg.Request, msg.Route.Uri
			msg.Request = msg.Route.Uri
			msg.Route = nil
		} else {
			seclast := msg.Route
			for ; seclast.Next.Next != nil; seclast = seclast.Next {
			}
			oldReq, newReq = msg.Request, seclast.Next.Uri
			msg.Request = seclast.Next.Uri
			seclast.Next = nil
			msg.Route.Last()
		}
		log.Printf("Fixing request URI after strict router traversal: %s -> %s", oldReq, newReq)
	}
}

func (tp *Transport) route(old *Msg) (msg *Msg, saddr string, err error) {
	var host string
	var port uint16
	msg = new(Msg)
	*msg = *old // Start off with a shallow copy.
	if msg.IsResponse {
		msg.Via = old.Via.Copy()
		if msg.Via.CompareAddr(tp.Via) {
			// In proxy scenarios we have to remove our own Via.
			msg.Via = msg.Via.Next
		}
		if msg.Via == nil {
			return nil, "", errors.New("Message missing Via header")
		}
		if received, ok := msg.Via.Params["received"]; ok {
			return msg, received, nil
		} else {
			host, port = msg.Via.Host, msg.Via.Port
		}
	} else {
		if msg.Request == nil {
			return nil, "", errors.New("Missing request URI")
		}
		if !msg.Via.CompareAddr(tp.Via) {
			return nil, "", errors.New("You forgot to say: msg.Via = tp.Via(msg.Via)")
		}
		if msg.Route != nil {
			if msg.Method == "REGISTER" {
				return nil, "", errors.New("Don't loose route register requests")
			}
			if _, ok := msg.Route.Uri.Params["lr"]; ok {
				// RFC3261 16.12.1.1 Basic SIP Trapezoid
				route := msg.Route
				msg.Route = msg.Route.Next
				host, port = route.Uri.Host, route.Uri.Port
			} else {
				// RFC3261 16.12.1.2: Traversing a Strict-Routing Proxy
				msg.Route = old.Route.Copy()
				msg.Route.Last().Next = &Addr{Uri: msg.Request}
				msg.Request = msg.Route.Uri
				msg.Route = msg.Route.Next
				host, port = msg.Request.Host, msg.Request.Port
			}
		} else {
			host, port = msg.Request.Host, msg.Request.Port
		}
	}
	saddr = util.HostPortToString(host, port)
	return
}

func addReceived(msg *Msg, addr *net.UDPAddr) {
	if msg.Via.Host != addr.IP.String() || int(msg.Via.Port) != addr.Port {
		msg.Via.Params["received"] = addr.String()
	}
}

func addTimestamp(msg *Msg, ts time.Time) {
	if *timestampTagging {
		msg.Via.Params["µsi"] = strconv.FormatInt(ts.UnixNano()/int64(time.Microsecond), 10)
	}
}