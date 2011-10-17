package imap

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
)

func init() {
	log.SetFlags(log.Ltime | log.Lshortfile)
}

func check(err os.Error) {
	if err != nil {
		panic(err)
	}
}

type Status int

const (
	OK Status = iota
	NO
	BAD
)

func (s Status) String() string {
	return []string{
		"OK",
		"NO",
		"BAD",
	}[s]
}

type Response struct {
	status Status
	code   string
	text   string
	extra  []interface{}
}

func (r *Response) String() string {
	return fmt.Sprintf("%s [%s] %s", r.status, r.code, r.text)
}

type IMAPError struct {
	status Status
	text   string
}

func (e *IMAPError) String() string {
	return fmt.Sprintf("%s %s", e.status, e.text)
}

const (
	WildcardAny          = "%"
	WildcardAnyRecursive = "*"
)

type TriBool int

const (
	TriUnknown = TriBool(iota)
	TriTrue
	TriFalse
)

func (t TriBool) String() string {
	switch t {
	case TriTrue:
		return "true"
	case TriFalse:
		return "false"
	}
	return "unknown"
}

type tag int

const Untagged = tag(-1)

type IMAP struct {
	// Client thread.
	nextTag int

	Unsolicited chan interface{}

	// Background thread.
	r *Parser
	w io.Writer

	lock    sync.Mutex
	pending map[tag]chan *Response
}

func NewIMAP() *IMAP {
	return &IMAP{pending: make(map[tag]chan *Response)}
}

func (imap *IMAP) Connect(hostport string) (string, os.Error) {
	conn, err := tls.Dial("tcp", hostport, nil)
	if err != nil {
		return "", err
	}

	imap.r = newParser(conn) //&LoggingReader{conn})
	imap.w = conn

	tag, err := imap.readTag()
	if err != nil {
		return "", err
	}
	if tag != Untagged {
		return "", fmt.Errorf("expected untagged server hello. got %q", tag)
	}

	resp, err := imap.readStatus("")
	if err != nil {
		return "", err
	}
	if resp.status != OK {
		return "", &IMAPError{resp.status, resp.text}
	}

	imap.StartLoops()

	return resp.text, nil
}

func (imap *IMAP) readTag() (tag, os.Error) {
	str, err := imap.r.readToken()
	if err != nil {
		return Untagged, err
	}
	if len(str) == 0 {
		return Untagged, os.NewError("read empty tag")
	}

	switch str[0] {
	case '*':
		return Untagged, nil
	case 'a':
		tagnum, err := strconv.Atoi(str[1:])
		if err != nil {
			return Untagged, err
		}
		return tag(tagnum), nil
	}

	return Untagged, fmt.Errorf("unexpected response %q", str)
}

func (imap *IMAP) Send(ch chan *Response, format string, args ...interface{}) os.Error {
	tag := tag(imap.nextTag)
	imap.nextTag++

	toSend := []byte(fmt.Sprintf("a%d %s\r\n", int(tag), fmt.Sprintf(format, args...)))

	if ch != nil {
		imap.lock.Lock()
		imap.pending[tag] = ch
		imap.lock.Unlock()
	}

	_, err := imap.w.Write(toSend)
	return err
}

func (imap *IMAP) SendSync(format string, args ...interface{}) (*Response, os.Error) {
	ch := make(chan *Response, 1)
	err := imap.Send(ch, format, args...)
	if err != nil {
		return nil, err
	}
	response := <-ch
	if response.status != OK {
		return nil, &IMAPError{response.status, response.text}
	}
	return response, nil
}

func (imap *IMAP) Auth(user string, pass string) (string, os.Error) {
	resp, err := imap.SendSync("LOGIN %s %s", user, pass)
	if err != nil {
		return "", err
	}
	for _, extra := range resp.extra {
		imap.Unsolicited <- extra
	}
	return resp.text, nil
}

func quote(in string) string {
	if strings.IndexAny(in, "\r\n") >= 0 {
		panic("invalid characters in string to quote")
	}
	return "\"" + in + "\""
}

func (imap *IMAP) List(reference string, name string) ([]*ResponseList, os.Error) {
	/* Responses:  untagged responses: LIST */
	response, err := imap.SendSync("LIST %s %s", quote(reference), quote(name))
	if err != nil {
		return nil, err
	}

	lists := make([]*ResponseList, 0)
	for _, extra := range response.extra {
		if list, ok := extra.(*ResponseList); ok {
			lists = append(lists, list)
		} else {
			imap.Unsolicited <- extra
		}
	}

	return lists, nil
}

type ResponseExamine struct {
	flags  []string
	exists int
	recent int
}

func (imap *IMAP) Examine(mailbox string) (*ResponseExamine, os.Error) {
	/*
	 Responses:  REQUIRED untagged responses: FLAGS, EXISTS, RECENT
	 REQUIRED OK untagged responses:  UNSEEN,  PERMANENTFLAGS,
	 UIDNEXT, UIDVALIDITY
	*/
	resp, err := imap.SendSync("EXAMINE %s", quote(mailbox))
	if err != nil {
		return nil, err
	}

	r := &ResponseExamine{}

	for _, extra := range resp.extra {
		switch extra := extra.(type) {
		case (*ResponseFlags):
			r.flags = extra.flags
		case (*ResponseExists):
			r.exists = extra.count
		case (*ResponseRecent):
			r.recent = extra.count
		//case (*Response):
		/*
		 // XXX parse tags
		*/
		default:
			imap.Unsolicited <- extra
		}
	}
	return r, nil
}

func (imap *IMAP) Fetch(sequence string, fields []string) ([]*ResponseFetch, os.Error) {
	var fieldsStr string
	if len(fields) == 1 {
		fieldsStr = fields[0]
	} else {
		fieldsStr = "(" + strings.Join(fields, " ") + ")"
	}
	resp, err := imap.SendSync("FETCH %s %s", sequence, fieldsStr)
	if err != nil {
		return nil, err
	}

	lists := make([]*ResponseFetch, 0)
	for _, extra := range resp.extra {
		if list, ok := extra.(*ResponseFetch); ok {
			lists = append(lists, list)
		} else {
			imap.Unsolicited <- extra
		}
	}
	return lists, nil
}

func (imap *IMAP) StartLoops() {
	go func() {
		err := imap.ReadLoop()
		panic(err)
	}()
}

func (imap *IMAP) ReadLoop() os.Error {
	var untagged []interface{}
	for {
		tag, err := imap.readTag()
		if err != nil {
			return err
		}

		if tag == Untagged {
			resp, err := imap.readUntagged()
			if err != nil {
				return err
			}

			if untagged == nil {
				imap.lock.Lock()
				hasPending := len(imap.pending) > 0
				imap.lock.Unlock()

				if hasPending {
					untagged = make([]interface{}, 0, 1)
				}
			}

			if untagged != nil {
				untagged = append(untagged, resp)
			} else {
				imap.Unsolicited <- resp
			}
		} else {
			resp, err := imap.readStatus("")
			if err != nil {
				return err
			}
			resp.extra = untagged

			imap.lock.Lock()
			ch := imap.pending[tag]
			imap.pending[tag] = nil, false
			imap.lock.Unlock()

			ch <- resp
			untagged = nil
		}
	}

	panic("not reached")
}

func (imap *IMAP) readStatus(statusStr string) (*Response, os.Error) {
	if len(statusStr) == 0 {
		var err os.Error
		statusStr, err = imap.r.readToken()
		if err != nil {
			return nil, err
		}
	}

	statusStrs := map[string]Status{
		"OK":  OK,
		"NO":  NO,
		"BAD": BAD,
	}

	status, known := statusStrs[statusStr]
	if !known {
		return nil, fmt.Errorf("unexpected status %q", statusStr)
	}

	peek, err := imap.r.Peek(1)
	if err != nil {
		return nil, err
	}
	var code string
	if peek[0] == '[' {
		code, err = imap.r.readBracketed()
		if err != nil {
			return nil, err
		}

		/*
		 resp-text-code  = "ALERT" /
		 "BADCHARSET" [SP "(" astring *(SP astring) ")" ] /
		 capability-data / "PARSE" /
		 "PERMANENTFLAGS" SP "("
		 [flag-perm *(SP flag-perm)] ")" /
		 "READ-ONLY" / "READ-WRITE" / "TRYCREATE" /
		 "UIDNEXT" SP nz-number / "UIDVALIDITY" SP nz-number /
		 "UNSEEN" SP nz-number /
		 atom [SP 1*<any TEXT-CHAR except "]">]
		*/

		err = imap.r.expect(" ")
		if err != nil {
			return nil, err
		}
	}

	rest, err := imap.r.readToEOL()
	if err != nil {
		return nil, err
	}

	return &Response{status, code, rest, nil}, nil
}

type ResponseCapabilities struct {
	caps []string
}

type ResponseList struct {
	Inferiors  TriBool
	Selectable TriBool
	Marked     TriBool
	Children   TriBool
	Delim      string
	Name       string
}

type ResponseFlags struct {
	flags []string
}

type ResponseExists struct {
	count int
}
type ResponseRecent struct {
	count int
}

type Address struct {
	name, source, address string
}

func (a *Address) FromSexp(s []Sexp) {
	if name := nilOrString(s[0]); name != nil {
		a.name = *name
	}
	if source := nilOrString(s[1]); source != nil {
		a.source = *source
	}
	mbox := nilOrString(s[2])
	host := nilOrString(s[3])
	if mbox != nil && host != nil {
		address := *mbox + "@" + *host
		a.address = address
	}
}
func AddressListFromSexp(s Sexp) []Address {
	if s == nil {
		return nil
	}

	saddrs := s.([]Sexp)
	addrs := make([]Address, len(saddrs))
	for i, s := range saddrs {
		addrs[i].FromSexp(s.([]Sexp))
	}
	return addrs
}

type ResponseFetchEnvelope struct {
	date, subject, inReplyTo, messageId *string
	from, sender, replyTo, to, cc, bcc  []Address
}

type ResponseFetch struct {
	Msg          int
	Flags        Sexp
	Envelope     ResponseFetchEnvelope
	InternalDate string
	Size         int
	Rfc822, Rfc822Header []byte
}

func (imap *IMAP) readCAPABILITY() *ResponseCapabilities {
	caps := make([]string, 0)
	for {
		cap, err := imap.r.readToken()
		check(err)
		if len(cap) == 0 {
			break
		}
		caps = append(caps, cap)
	}
	check(imap.r.expectEOL())
	return &ResponseCapabilities{caps}
}

func (imap *IMAP) readLIST() *ResponseList {
	// "(" [mbx-list-flags] ")" SP (DQUOTE QUOTED-CHAR DQUOTE / nil) SP mailbox
	flags, err := imap.r.readParenStringList()
	check(err)
	imap.r.expect(" ")

	delim, err := imap.r.readQuoted()
	check(err)
	imap.r.expect(" ")

	name, err := imap.r.readQuoted()
	check(err)

	check(imap.r.expectEOL())

	list := &ResponseList{Delim: string(delim), Name: string(name)}
	for _, flag := range flags {
		switch flag {
		case "\\Noinferiors":
			list.Inferiors = TriFalse
		case "\\Noselect":
			list.Selectable = TriFalse
		case "\\Marked":
			list.Marked = TriTrue
		case "\\Unmarked":
			list.Marked = TriFalse
		case "\\HasChildren":
			list.Children = TriTrue
		case "\\HasNoChildren":
			list.Children = TriFalse
		default:
			panic(fmt.Sprintf("unknown list flag %q", flag))
		}
	}
	return list
}

func (imap *IMAP) readFLAGS() *ResponseFlags {
	flags, err := imap.r.readParenStringList()
	check(err)
	check(imap.r.expectEOL())
	return &ResponseFlags{flags}
}

func (imap *IMAP) readFETCH(num int) *ResponseFetch {
	sexp, err := imap.r.readSexp()
	check(err)
	if len(sexp)%2 != 0 {
		panic("fetch sexp must have even number of items")
	}
	fetch := &ResponseFetch{Msg: num}
	for i := 0; i < len(sexp); i += 2 {
		key := sexp[i].(string)
		switch key {
		case "ENVELOPE":
			env := sexp[i+1].([]Sexp)
			// This format is insane.
			if len(env) != 10 {
				panic(fmt.Sprintf("envelope needed 10 fields, had %d", len(env)))
			}
			fetch.Envelope.date = nilOrString(env[0])
			fetch.Envelope.subject = nilOrString(env[1])
			fetch.Envelope.from = AddressListFromSexp(env[2])
			fetch.Envelope.sender = AddressListFromSexp(env[3])
			fetch.Envelope.replyTo = AddressListFromSexp(env[4])
			fetch.Envelope.to = AddressListFromSexp(env[5])
			fetch.Envelope.cc = AddressListFromSexp(env[6])
			fetch.Envelope.bcc = AddressListFromSexp(env[7])
			fetch.Envelope.inReplyTo = nilOrString(env[8])
			fetch.Envelope.messageId = nilOrString(env[9])
		case "FLAGS":
			fetch.Flags = sexp[i+1]
		case "INTERNALDATE":
			fetch.InternalDate = sexp[i+1].(string)
		case "RFC822":
			fetch.Rfc822 = sexp[i+1].([]byte)
		case "RFC822.HEADER":
			fetch.Rfc822Header = sexp[i+1].([]byte)
		case "RFC822.SIZE":
			fetch.Size, err = strconv.Atoi(sexp[i+1].(string))
			check(err)
		default:
			panic(fmt.Sprintf("unhandled fetch key %#v", key))
		}
	}
	check(imap.r.expectEOL())
	return fetch
}

func (imap *IMAP) readUntagged() (resp interface{}, outErr os.Error) {
	defer func() {
		if e := recover(); e != nil {
			if osErr, ok := e.(os.Error); ok {
				outErr = osErr
				return
			}
			panic(e)
		}
	}()

	command, err := imap.r.readToken()
	check(err)

	switch command {
	case "CAPABILITY":
		return imap.readCAPABILITY(), nil
	case "LIST":
		return imap.readLIST(), nil
	case "FLAGS":
		return imap.readFLAGS(), nil
	case "OK", "NO", "BAD":
		resp, err := imap.readStatus(command)
		check(err)
		return resp, nil
	}

	num, err := strconv.Atoi(command)
	if err == nil {
		command, err := imap.r.readToken()
		check(err)

		switch command {
		case "EXISTS":
			check(imap.r.expectEOL())
			return &ResponseExists{num}, nil
		case "RECENT":
			check(imap.r.expectEOL())
			return &ResponseRecent{num}, nil
		case "FETCH":
			return imap.readFETCH(num), nil
		}
	}

	return nil, fmt.Errorf("unhandled untagged response %s", command)
}