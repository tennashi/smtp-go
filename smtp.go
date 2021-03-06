package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/textproto"
	"strings"
	"time"
)

// ErrCRLFContain is returned when command text containing CR or LF.
var ErrCRLFContain = errors.New("smtp: A line must not contain CR or LF")

// Client represents a client connection an SMTP server.
type Client struct {
	remoteHost string
	localHost  string
	ext        map[string]string
	auth       []string
}

// NewClient returns new Client.
func NewClient(host string) *Client {
	return &Client{
		localHost:  "localhost",
		remoteHost: host,
	}
}

type conn struct {
	textConn *textproto.Conn
	netConn  net.Conn
}

func (c *conn) close() error {
	if err := c.textConn.Close(); err != nil {
		return err
	}
	return c.netConn.Close()
}

func (c *conn) execCmd(expectCode int, fmt string, args ...interface{}) (int, string, error) {
	id, err := c.textConn.Cmd(fmt, args...)
	if err != nil {
		return 0, "", err
	}
	c.textConn.StartResponse(id)
	defer c.textConn.EndResponse(id)
	return c.textConn.ReadResponse(expectCode)
}

func validateLine(line string) error {
	if strings.ContainsAny(line, "\r\n") {
		return ErrCRLFContain
	}
	return nil
}

func (c *Client) dial() (*conn, error) {
	var netConn net.Conn
	host, port, err := net.SplitHostPort(c.remoteHost)
	if err != nil {
		return nil, err
	}
	var mxRecords []*net.MX
	var tempDelay time.Duration
	for {
		var err error
		mxRecords, err = net.LookupMX(host)
		if err == nil {
			break
		}
		var de *net.DNSError
		if !errors.As(err, &de) {
			return nil, err
		}
		if de.Temporary() {
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if tempDelay > 1*time.Second {
				tempDelay = 1 * time.Second
			}
			time.Sleep(tempDelay)
			continue
		}
		if de.Timeout() {
			return nil, err
		}
		if de.IsNotFound {
			mxRecords = []*net.MX{{Host: host}}
			break
		}
	}
	for _, mx := range mxRecords {
		netConn, err = net.Dial("tcp", mx.Host+":"+port)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	textConn := textproto.NewConn(netConn)
	if _, _, err := textConn.ReadCodeLine(220); err != nil {
		return nil, err
	}
	return &conn{
		textConn: textConn,
		netConn:  netConn,
	}, nil
}

func (c *Client) hello(conn *conn, localHost string) error {
	if err := validateLine(localHost); err != nil {
		return err
	}
	_, msg, err := conn.execCmd(250, "EHLO %s", localHost)
	if err != nil {
		if _, _, err := conn.execCmd(250, "HELO %s", localHost); err != nil {
			return err
		}
		return nil
	}
	ext := parseExt(msg)
	if auth, ok := ext["AUTH"]; ok {
		c.auth = strings.Split(auth, " ")
	}
	c.ext = ext
	c.localHost = localHost
	return nil
}

func parseExt(ehloMsg string) map[string]string {
	extMsgs := strings.Split(ehloMsg, "\n")
	ext := make(map[string]string, len(extMsgs)-1)
	if len(extMsgs) > 1 {
		extMsgs = extMsgs[1:]
		for _, extMsg := range extMsgs {
			extKV := strings.SplitN(extMsg, " ", 2)
			if len(extKV) > 1 {
				ext[extKV[0]] = extKV[1]
				break
			}
			ext[extKV[0]] = ""
		}
	}
	return ext
}

func (c *Client) mail(conn *conn, from string) error {
	cmdStr := "MAIL FROM:<%s>"
	if c.ext != nil {
		if _, ok := c.ext["8BITMIME"]; ok {
			cmdStr += " BODY=8BITMIME"
		}
	}
	_, _, err := conn.execCmd(250, cmdStr, from)
	return err
}

func (c *Client) startTLS(conn *conn, config *tls.Config) error {
	if _, _, err := conn.execCmd(220, "STARTTLS"); err != nil {
		return err
	}
	conn.netConn = tls.Client(conn.netConn, config)
	conn.textConn = textproto.NewConn(conn.netConn)
	return c.hello(conn, c.localHost)
}

// Send sends an email with the request r.
func (c *Client) Send(r *Request) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.close()

	if err := c.hello(conn, c.localHost); err != nil {
		return err
	}

	if _, ok := c.ext["STARTTLS"]; ok && r.StartTLS {
		tlsCfg := r.TLSConfig
		if tlsCfg == nil {
			tlsCfg = &tls.Config{ServerName: c.remoteHost}
		}
		if err := c.startTLS(conn, tlsCfg); err != nil {
			return err
		}
	}

	if err := c.mail(conn, r.From); err != nil {
		return err
	}

	for _, to := range r.To {
		if _, _, err := conn.execCmd(25, "RCPT TO:<%s>", to); err != nil {
			return err
		}
	}
	for _, bcc := range r.Bcc {
		if _, _, err := conn.execCmd(25, "RCPT TO:<%s>", bcc); err != nil {
			return err
		}
	}

	if _, _, err := conn.execCmd(354, "DATA"); err != nil {
		return err
	}

	w := conn.textConn.DotWriter()
	if err := r.Write(w); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if _, _, err := conn.textConn.ReadResponse(250); err != nil {
		return err
	}

	if _, _, err := conn.execCmd(221, "QUIT"); err != nil {
		return err
	}
	return nil
}

// Request represents an mail request.
type Request struct {
	From      string
	To        []string
	Cc        []string
	Bcc       []string
	Subject   string
	Header    Header
	Body      io.ReadCloser
	StartTLS  bool
	TLSConfig *tls.Config
	ctx       context.Context
}

// NewRequest returns new Request.
func NewRequest(ctx context.Context, to []string, body io.Reader) (*Request, error) {
	// TODO: validate `to`
	if ctx == nil {
		ctx = context.Background()
	}
	r, ok := body.(io.ReadCloser)
	if !ok {
		r = ioutil.NopCloser(body)
	}
	return &Request{
		To:     to,
		Body:   r,
		ctx:    ctx,
		Header: map[string][]string{},
	}, nil
}

func (r *Request) Write(w io.Writer) error {
	if err := writeHeader(w, "From", r.From); err != nil {
		return err
	}
	for _, to := range r.To {
		if err := writeHeader(w, "To", to); err != nil {
			return err
		}
	}
	for _, cc := range r.Cc {
		if err := writeHeader(w, "Cc", cc); err != nil {
			return err
		}
	}
	if err := writeHeader(w, "Subject", r.Subject); err != nil {
		return err
	}
	if err := r.Header.WriteSubset(w, defaultExcludeHeaders); err != nil {
		return err
	}
	if _, err := io.Copy(w, r.Body); err != nil {
		return err
	}
	return nil
}

// Header represents the key-value pairs in an SMTP header.
type Header map[string][]string

var headerNewlineToSpace = strings.NewReplacer("\n", " ", "\r", " ")
var defaultExcludeHeaders = map[string]bool{
	"From":    true,
	"To":      true,
	"Subject": true,
}

// Add adds the key, value pair to the header.
func (h Header) Add(key, value string) {
	textproto.MIMEHeader(h).Add(key, value)
}

// Del deletes the values associated with key.
func (h Header) Del(key string) {
	textproto.MIMEHeader(h).Del(key)
}

// Get gets the first value associated with the given key.
func (h Header) Get(key string) string {
	return textproto.MIMEHeader(h).Get(key)
}

// Set sets the header entries associated with key to the single element value.
func (h Header) Set(key, value string) {
	textproto.MIMEHeader(h).Set(key, value)
}

// Write writes a header in wire format.
func (h Header) Write(w io.Writer) error {
	return h.WriteSubset(w, nil)
}

// WriteSubset writes a header in wire format.
// If exclude is not nil, keys where exclude[key] == true are not written.
func (h Header) WriteSubset(w io.Writer, exclude map[string]bool) error {
	h.exclude(exclude)
	for k, vs := range h {
		for _, v := range vs {
			v = headerNewlineToSpace.Replace(v)
			v = textproto.TrimString(v)
			if err := writeHeader(w, k, v); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(w, "\r\n")
	return err
}

func (h Header) exclude(exclude map[string]bool) {
	for k := range h {
		if exclude[k] {
			delete(h, k)
		}
	}
}

func writeHeader(w io.Writer, key, value string) error {
	_, err := fmt.Fprintf(w, "%s: %s\r\n", key, value)
	return err
}
