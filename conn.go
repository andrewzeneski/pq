package pq

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"runtime"
	"strings"
	"time"
)

var (
	ErrSSLNotSupported = errors.New("SSL is not enabled on the server")
)

const timeFormat = "2006-01-02 15:04:05.000000-07"

type h struct {
	T int8
	L int32
}

type msg struct {
	*h
	b *bytes.Buffer
}

func newMsg() *msg {
	return &msg{h: new(h), b: new(bytes.Buffer)}
}

func (m *msg) setHead(t int8) {
	if m.b.Len() != 0 {
		panic(errf("attempt to setHead('%c') with %d byte(s) in buffer: %q", t, m.b.Len(), m.b))
	}
	m.T = t
}

func (m *msg) readCString() string {
	l, err := m.b.ReadString(0)
	if err != nil {
		panic(err)
	}

	if len(l) == 0 {
		return ""
	}

	return l[:len(l)-1]
}

func (m *msg) read(x interface{}) {
	err := binary.Read(m.b, binary.BigEndian, x)
	if err != nil {
		panic(err)
	}
	return
}

func (m *msg) write(x ...interface{}) {
	for _, o := range x {
		switch v := o.(type) {
		case string:
			_, err := io.WriteString(m.b, v+"\000")
			if err != nil {
				panic(err)
			}
		default:
			err := binary.Write(m.b, binary.BigEndian, v)
			if err != nil {
				panic(err)
			}
		}
	}
}

func (m *msg) writeTo(w io.Writer) {
	m.L = int32(m.b.Len() + 4)

	var x interface{} = m.h

	if m.T == 0 {
		x = m.L
	}

	err := binary.Write(w, binary.BigEndian, x)
	if err != nil {
		panic(err)
	}

	_, err = m.b.WriteTo(w)
	if err != nil {
		panic(err)
	}
}

func (m *msg) readFrom(r io.Reader) {
	err := binary.Read(r, binary.BigEndian, m.h)
	if err != nil {
		panic(err)
	}

	_, err = io.CopyN(m.b, r, int64(m.L-4))
	if err != nil {
		panic(err)
	}
}

type Values map[string]string

func (vs Values) Get(k string) (v string) {
	v, _ = vs[k]
	return v
}

func (vs Values) Set(k, v string) {
	vs[k] = v
}

type pgdriver struct{}

func (*pgdriver) Open(name string) (driver.Conn, error) {
	return Open(name)
}

func init() {
	sql.Register("postgres", &pgdriver{})
}

type stateFn func(cn *Conn) stateFn

type Conn struct {
	c net.Conn
	*msg
	cid    int32
	pid    int32
	status byte
}

func Open(name string) (cn *Conn, err error) {
	defer recoverErr(&err)

	// TODO: less naive parsing.
	// See: http://www.postgresql.org/docs/7.4/static/libpq.html#LIBPQ-CONNECT
	o, err := parseConnString(name)
	if err != nil {
		return nil, err
	}

	c, err := dial(o)
	if err != nil {
		return nil, err
	}

	cn = &Conn{c: c, msg: newMsg()}
	cn.ssl(o)
	cn.startup(o)

	return
}

func (cn *Conn) ssl(o Values) {
	tlsConf := tls.Config{}
	switch o.Get("sslmode") {
	case "require", "":
		tlsConf.InsecureSkipVerify = true
	case "verify-full":
		// fall out
	case "disable":
		return
	default:
		panic(errf(`unsupported sslmode %q; only "require" (default), "verify-full", and "disable" supported`))
	}

	cn.setHead(0)
	cn.write(int32(80877103))
	cn.sendMsg()

	b := make([]byte, 1)
	_, err := io.ReadFull(cn.c, b)
	if err != nil {
		panic(err)
	}

	if b[0] != 'S' {
		panic(ErrSSLNotSupported)
	}

	cn.c = tls.Client(cn.c, &tlsConf)
}

func (cn *Conn) startup(o Values) {
	cn.setHead(0)
	cn.write(int32(196608))
	cn.write("user", o.Get("user"))
	cn.write("database", o.Get("dbname"))
	cn.write("")
	cn.sendMsg()

	for {
		cn.recvMsg()
		switch cn.T {
		case 'R':
			cn.auth(o)
		case 'S':
			// Ignore these for now
			cn.readCString()
			cn.readCString()
		case 'K':
			cn.read(&cn.cid)
			cn.read(&cn.pid)
		case 'Z':
			cn.read(&cn.status)
			return
		default:
			panic(errf("unknown response for startup: '%c'", cn.T))
		}
	}

	return
}

func (cn *Conn) auth(o Values) {
	var code int32
	cn.read(&code)
	switch code {
	case 0: // OK
		return
	case 5: // MD5
		salt := make([]byte, 4)
		cn.read(salt)
		// in SQL: concat('md5', md5(concat(md5(concat(password, username)), random-salt)))
		sum := "md5" + md5s(md5s(o.Get("password")+o.Get("user"))+string(salt))
		cn.setHead('p')
		cn.write(sum)
		cn.sendMsg()

		cn.recvMsg()
		if cn.T != 'R' {
			panic(errf("unknown response for password message: '%c'", cn.T))
		}

		cn.read(&code)
		if code == 0 {
			return
		}
	}

	panic(errf("unknown response for authentication: '%d'", code))
}

func md5s(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (cn *Conn) Close() error {
	return cn.c.Close()
}

func (cn *Conn) Rollback() (err error) {
	s, err := cn.Prepare("ROLLBACK")
	if err != nil {
		return err
	}
	defer s.Close()

	_, err = s.Query(nil)
	if err != nil {
		return err
	}
	return
}

func (cn *Conn) Commit() (err error) {
	s, err := cn.Prepare("COMMIT")
	if err != nil {
		return err
	}
	defer s.Close()

	_, err = s.Query(nil)
	if err != nil {
		return err
	}
	return
}

func (cn *Conn) Begin() (tx driver.Tx, err error) {
	// TODO: maybe cache stmt to avoid repreparing?
	s, err := cn.Prepare("BEGIN")
	if err != nil {
		return nil, err
	}
	defer s.Close()

	_, err = s.Query(nil)
	if err != nil {
		return nil, err
	}

	return cn, err
}

func (cn *Conn) Prepare(q string) (st driver.Stmt, err error) {
	defer recoverErr(&err)

	cn.setHead('P')
	cn.write("")
	cn.write(q)
	cn.write(int16(0))
	cn.sendMsg()

	cn.setHead('S')
	cn.sendMsg()

	cn.recvMsg()
	if cn.T != '1' {
		panic(errf("unknown response from parse: '%c'", cn.T))
	}

	cn.recvMsg()
	if cn.T != 'Z' {
		panic(errf("unknown response from parse: '%c'", cn.T))
	}
	cn.read(&cn.status)

	return &stmt{Conn: cn}, nil
}

func (cn *Conn) sendMsg() {
	cn.writeTo(cn.c)
}

func (cn *Conn) recvMsg() {
	cn.readFrom(cn.c)
	if cn.T == 'E' {
		panic(readError(cn))
	}
}

type stmt struct {
	*Conn
	q string
}

// Need to talk with bradfitz about this before implementing these.
func (st *stmt) Close() error                                 { return nil }
func (st *stmt) NumInput() int                                { return -1 }
func (st *stmt) Exec(v []driver.Value) (driver.Result, error) { panic("todo") }

func (st *stmt) Query(v []driver.Value) (r driver.Rows, err error) {
	defer recoverErr(&err)

	st.setHead('D')
	st.write(byte('S'))
	st.write("")
	st.sendMsg()

	st.setHead('B')
	st.write("")
	st.write("")
	st.write(int16(0))
	st.write(int16(len(v)))
	for _, v := range v {
		l, s := encodeParam(v)
		st.write(l, s)
	}
	st.write(int16(0))
	st.sendMsg()

	st.setHead('E')
	st.write("")
	st.write(int32(0))
	st.sendMsg()

	st.setHead('S')
	st.sendMsg()

	st.recvParameterDescription()
	col := st.recvRowDescription()

	st.recvMsg()
	if st.T != '2' {
		panic(errf("unknown response for bind: '%c'", st.T))
	}

	return &rows{col: col, Conn: st.Conn}, nil
}

func (st *stmt) recvParameterDescription() {
	// Assert then ignore this message for now
	st.recvMsg()
	if st.T != 't' {
		panic(errf("expected parameter description, got: '%c'", st.T))
	}
	st.msg = newMsg()
}

func (st *stmt) recvRowDescription() []string {
	st.recvMsg()
	if st.T != 'T' {
		panic(errf("expected row description, got: '%c'", st.T))
	}

	var n int16
	st.read(&n)

	col := make([]string, n)
	for i := 0; i < len(col); i++ {
		col[i] = st.readCString()
		st.msg.b.Next(18) // Throw away unwanted (for now) fields.
	}

	return col
}

type rows struct {
	*Conn
	col []string
	done bool
}

func (r *rows) Columns() []string {
	return r.col
}

func (r *rows) Close() error {
	defer func() {
		r.msg = newMsg()
	}()

	// TODO: Should I be doing this? Ask bradfitz.
	//    NOTE: QueryRow doesn't work without this because it never reads until EOF
	//    and so there is still a 'C' waiting in the pipe.
	for {
		err := r.Next(nil)
		switch err {
		case nil:
		case io.EOF:
			return nil
		default:
			return err
		}

		// Throw away messages we don't care about
	}
	return nil
}

func (r *rows) Next(dest []driver.Value) (err error) {
	if r.done {
		return io.EOF
	}

	defer recoverErr(&err)

	r.recvMsg()
	switch {
	case r.T == 'C':
		r.recvMsg()
		if r.T != 'Z' {
			return errf("expected 'Z' but got: '%c'", r.T)
		}
		r.read(&r.status)
		r.done = true
		return io.EOF
	case r.T != 'D':
		return errf("unknown response for execute: '%c'", r.T)
	}

	var n int16
	var l int32

	r.read(&n)
	for i := int16(0); i < n; i++ {
		r.read(&l)
		if l < 0 { // nil
			dest[i] = nil
			continue
		}
		b := make([]byte, l)
		r.read(b)
		dest[i] = b
	}

	return nil
}

func recoverErr(err *error) {
	x := recover()
	if x == nil {
		return
	}

	switch v := x.(type) {
	case runtime.Error:
		panic(x)
	case error:
		*err = v
	default:
		panic(x)
	}
}

func dial(o Values) (net.Conn, error) {
	// TODO: support possible network types
	// See: http://www.postgresql.org/docs/7.4/static/libpq.html#LIBPQ-CONNECT
	host := o.Get("host")
	if strings.HasPrefix(host, "/") {
		return net.Dial("unix", host)
	}

	if host == "" {
		host = "localhost"
	}

	port := o.Get("port")
	if port == "" {
		port = "5432"
	}

	return net.Dial("tcp", host+":"+port)
}

func parseConnString(cs string) (Values, error) {
	o := make(Values)
	parts := strings.Split(cs, " ")
	for _, p := range parts {
		kv := strings.Split(p, "=")
		if len(kv) < 2 {
			return nil, errf("invalid connection option: %q", p)
		}
		o.Set(kv[0], kv[1])
	}
	return o, nil
}

func ParseURL(us string) (string, error) {
	u, err := url.Parse(us)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" {
		return "", fmt.Errorf("invalid connection protocol: %s", u.Scheme)
	}

	result := make([]string, 0, 5)
	host := ""
	switch i := strings.Index(u.Host, ":"); i {
	case -1:
		host = u.Host
	case 0:
		return "", fmt.Errorf("missing host")
	default:
		host = u.Host[:i]
		result = append(result, fmt.Sprintf("port=%s", u.Host[i+1:]))
	}
	result = append(result, fmt.Sprintf("host=%s", host))

	if u.User != nil {
		if un := u.User.Username(); un != "" {
			result = append(result, fmt.Sprintf("user=%s", un))
		}
		if p, set := u.User.Password(); set && p != "" {
			result = append(result, fmt.Sprintf("password=%s", p))
		}
	}

	if u.Path != "" && u.Path != "/" {
		result = append(result, fmt.Sprintf("dbname=%s", u.Path[1:]))
	}

	return strings.Join(result, " "), nil
}

func errf(s string, args ...interface{}) error {
	return fmt.Errorf("pq: "+s, args...)
}

func encodeParam(param interface{}) (int32, []byte) {
	var s string
	switch param.(type) {
	default:
		panic(fmt.Sprintf("unknown type for %T", param))
	case int, uint8, uint16, uint32, uint64, int8, int16, int32, int64:
		s = fmt.Sprintf("%d", param)
	case float32, float64:
		s = fmt.Sprintf("%f", param)
	case string, []byte:
		s = fmt.Sprintf("%s", param)
	case bool:
		s = fmt.Sprintf("%t", param)
	case time.Time:
		s = param.(time.Time).Format(timeFormat)
	case nil:
		return -1, []byte{}
	}

	return int32(len(s)), []byte(s)
}

type ErrorFields map[byte]string

type ServerError struct {
	Fields ErrorFields
}

func (err *ServerError) Error() (s string) {
	for k, v := range err.Fields {
		s += fmt.Sprintf(` '%c':%s`, k, v)
	}
	return
}

func readError(cn *Conn) (err error) {
	defer recoverErr(&err)

	e := &ServerError{Fields: make(ErrorFields)}
	var t byte
	for {
		cn.read(&t)
		if t == 0 {
			break
		}
		e.Fields[t] = cn.readCString()
	}

	return e
}
