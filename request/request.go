// Package request contains functions to build an HTTP request from a template.
package request

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/pflag"
)

// Header is an HTTP header that implements the pflag.Value interface.
type Header struct {
	Header http.Header
	Remove map[string]struct{} // entries are to be removed before sending the HTTP request
}

func (h Header) String() (s string) {
	for k, v := range h.Header {
		s += fmt.Sprintf(`"%v: %v", `, k, strings.Join(v, ","))
	}

	// if there's at least one value, strip the extra ", " from the end of the string
	if len(s) > 0 {
		s = strings.TrimSuffix(s, ", ")
	}
	return s
}

// Set allows setting an HTTP header via options and pflag.
func (h Header) Set(s string) error {
	// get name and value from s
	data := strings.SplitN(s, ":", 2)
	name := data[0]

	if len(data) == 1 {
		// no value specified, this means the header is to be removed
		h.Remove[name] = struct{}{}
		return nil
	}

	// otherwise we have a name: value pair
	val := data[1]

	// if the header is still at the default value, remove the default value first
	if headerDefaultValue(h, name) {
		delete(h.Header, name)
	}

	// strip the leading space if necessary
	if len(val) > 0 && val[0] == ' ' {
		val = val[1:]
	}

	// use original name in case there's a string we need to replace later
	h.Header[name] = append(h.Header[name], val)
	return nil
}

// Type returns a description string for header.
func (h Header) Type() string {
	return "name: value"
}

// NewHeader initializes a Header.
func NewHeader(defaults http.Header) *Header {
	hdr := make(http.Header)
	for k, vs := range defaults {
		hdr[k] = vs
	}
	return &Header{
		Header: hdr,
		Remove: make(map[string]struct{}),
	}
}

// Apply applies the values in h to the target http.Header. The function
// insertValue is called for all names and values before adding them.
func (h Header) Apply(hdr http.Header, insertValue func(string) string) {
	for k, vs := range h.Header {
		// don't set the header if it is already set in the request and the
		// value is the default one.
		if _, ok := hdr[k]; ok && headerDefaultValue(h, k) {
			continue
		}

		// remove value if present
		hdr.Del(k)

		// add values
		k = insertValue(k)
		for _, v := range vs {
			hdr.Add(k, insertValue(v))
		}
	}

	for k := range h.Remove {
		hdr.Del(k)
	}
}

func headerDefaultValue(h Header, name string) bool {
	key := textproto.CanonicalMIMEHeaderKey(name)

	v, ok := h.Header[key]
	if !ok {
		return false
	}

	def, ok := DefaultHeader[key]
	if !ok {
		return false
	}

	if len(v) != len(def) {
		return false
	}

	// make copies of the two slices to prevent modifying the original data by
	// sorting
	a := make([]string, len(v))
	copy(a, v)
	sort.Strings(a)

	b := make([]string, len(v))
	copy(b, def)
	sort.Strings(b)

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// DefaultHeader contains all HTTP header values that are added by default. If
// the header is already present, it is not added.
var DefaultHeader = http.Header{
	"Accept":     []string{"*/*"},
	"User-Agent": []string{"monsoon"},
}

// Request is a template for an HTTP request.
type Request struct {
	URL    string
	Method string
	Header *Header
	Body   string

	TemplateFile string

	ForceChunkedEncoding bool
}

// New returns a new request.
func New() *Request {
	return &Request{
		Header: NewHeader(DefaultHeader),
	}
}

// AddFlags adds flags for all options of a request to fs.
func (r *Request) AddFlags(fs *pflag.FlagSet) {
	// basics
	fs.StringVar(&r.Method, "request", "", "use HTTP request `method`")
	fs.MarkDeprecated("request", "use --method")
	fs.StringVarP(&r.Method, "method", "X", "", "use HTTP request `method`")
	fs.VarP(r.Header, "header", "H", "add `\"name: value\"` as an HTTP request header")
	fs.StringVarP(&r.Body, "data", "d", "", "transmit `data` in the HTTP request body")

	fs.StringVar(&r.TemplateFile, "template-file", "", "read HTTP request from `file`")

	// configure request
	fs.BoolVar(&r.ForceChunkedEncoding, "force-chunked-encoding", false, `do not set the Content-Length HTTP header and use chunked encoding`)
}

func replaceTemplate(s, template, value string) string {
	if !strings.Contains(s, template) {
		return s
	}

	return strings.Replace(s, template, value, -1)
}

// Apply replaces the template with value in all fields of the request and
// returns a new http.Request.
func (r *Request) Apply(template, value string) (*http.Request, error) {
	insertValue := func(s string) string {
		return replaceTemplate(s, template, value)
	}

	targetURL := insertValue(r.URL)
	body := []byte(insertValue(r.Body))

	var req *http.Request

	// if a template file is given, read the HTTP request from it as a basis
	if r.TemplateFile != "" {
		buf, err := ioutil.ReadFile(r.TemplateFile)
		if err != nil {
			return nil, err
		}

		// replace the placeholder in the file we just read
		buf = bytes.Replace(buf, []byte(template), []byte(value), -1)

		rd := bufio.NewReader(bytes.NewReader(buf))
		req, err = http.ReadRequest(rd)
		if err != nil {
			return nil, fmt.Errorf("error reading HTTP request from %v: %v", r.TemplateFile, err)
		}

		// append the rest of the file to the body
		rest, err := ioutil.ReadAll(rd)
		if err == io.EOF {
			// if nothing further can be read, that's fine with us
			err = nil
		}
		if err != nil {
			return nil, err
		}

		// rebuild body
		origBody, err := ioutil.ReadAll(req.Body)
		if err == io.ErrUnexpectedEOF {
			err = nil
		}
		if err != nil {
			return nil, err
		}

		origBody = append(origBody, rest...)
		req.Body = ioutil.NopCloser(bytes.NewReader(origBody))
		req.ContentLength = int64(len(origBody))

		// fill some details from the URL
		u, err := url.Parse(targetURL)
		if err != nil {
			return nil, err
		}

		// check that the URL does not contain too much information, only host,
		// port, and scheme are considered
		if u.Path != "" && u.Path != "/" {
			return nil, errors.New("URL must not contain a path, it's taken from the template file")
		}

		if u.RawQuery != "" {
			return nil, errors.New("URL must not contain a query string, it's taken from the template file")
		}

		req.URL.Scheme = u.Scheme
		req.URL.Host = u.Host

		if u.User != nil {
			req.URL.User = u.User
		}

		if len(body) > 0 {
			// use new body and set content length
			req.Body = ioutil.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		} else {
			// make sure the body is complete
		}

		if r.Method != "" {
			req.Method = insertValue(r.Method)
		}

	} else {
		var err error

		// create new request from scratch
		req, err = http.NewRequest(insertValue(r.Method), targetURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
	}

	if r.ForceChunkedEncoding {
		req.ContentLength = -1
	}

	if req.URL.User != nil {
		u := req.URL.User.Username()
		p, _ := req.URL.User.Password()
		req.SetBasicAuth(u, p)
	}

	// make sure there's a valid path
	if req.URL.Path == "" {
		req.URL.Path = "/"
	}

	// apply template headers
	r.Header.Apply(req.Header, insertValue)

	// special handling for the Host header, which needs to be set on the
	// request field Host
	for k, v := range r.Header.Header {
		if textproto.CanonicalMIMEHeaderKey(k) == "Host" {
			req.Host = v[0]
		}
	}

	for k := range r.Header.Remove {
		name := textproto.CanonicalMIMEHeaderKey(k)

		// special handling for sending a request without any user-agent header:
		// it must be set to the empty string in the http.Request.Header to prevent
		// Go stdlib from setting the default user agent
		if name == "User-Agent" {
			req.Header.Set("User-Agent", "")
		}

		// known limitation: due to the way the Go stdlib handles setting the
		// user-agent header, it's currently not possible to send a request with
		// multiple user-agent headers.

		// special handling if the Host header is to be removed
		if name == "Host" {
			return nil, errors.New("request without Host header is not supported")
		}
	}

	return req, nil
}
