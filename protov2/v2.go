package protov2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/storage"
)

type ObjectInfoRequest struct {
	Want string
	OID  []plumbing.Hash
}

func (r *ObjectInfoRequest) Encode(w io.Writer) error {
	e := pktline.NewEncoder(w)
	e.EncodeString("command=object-info\n")
	w.Write([]byte("0001"))
	e.EncodeString("size\n")
	for _, id := range r.OID {
		e.Encodef("oid %s\n", id)
	}
	e.Flush()
	return nil
}

type ObjectInfoSize struct {
	Hash plumbing.Hash
	Size uint64
}

type ObjectInfoResponse struct {
	Sizes []ObjectInfoSize
}

func (rep *ObjectInfoResponse) Decode(r io.Reader) error {
	d := pktline.NewScanner(r)
	d.Scan()

	if line := d.Bytes(); string(line) != "size" {
		return fmt.Errorf("got %q, want 'size'", line)
	}

	for d.Scan() {
		line := d.Bytes()
		if len(line) == 0 {
			continue
		}
		idx := bytes.IndexByte(line, ' ')
		if idx == -1 {
			continue
		}
		h := plumbing.NewHash(string(line[:idx]))
		line = line[idx+1:]
		if len(line) == 0 {
			continue
		}
		sz, err := strconv.ParseUint(string(line), 10, 64)
		if err != nil {
			return err
		}

		rep.Sizes = append(rep.Sizes, ObjectInfoSize{
			Hash: h,
			Size: sz,
		})
	}
	return nil
}

// pktline scanner
type scanner struct {
	buf []byte
	r   io.Reader
}

func newScanner(r io.Reader) *scanner {
	return &scanner{
		r:   r,
		buf: make([]byte, 4096),
	}
}

var flushPacket = []byte("0000")
var delimiterPacket = []byte("0001")

var ErrUnsupported = fmt.Errorf("unsupported git operation")

func packetString(b []byte) string {
	l := len(b)
	if l > 0 && b[l-1] == '\n' {
		l--
	}
	return string(b[:l])
}

func (s *scanner) readLine() (packet []byte, magic bool, err error) {
	if _, err := io.ReadFull(s.r, s.buf[:4]); err != nil {
		return nil, false, err
	}

	if string(s.buf[:4]) == "0000" {
		return flushPacket, true, nil
	}
	if string(s.buf[:4]) == "0001" {
		return delimiterPacket, true, nil
	}

	var len int
	if _, err := fmt.Sscanf(string(s.buf[:4]), "%x", &len); err != nil {
		return nil, false, err
	}

	len -= 4
	if cap(s.buf) < int(len) {
		s.buf = make([]byte, len)
	}
	if _, err := io.ReadFull(s.r, s.buf[:len]); err != nil {
		return nil, false, err
	}
	return s.buf[:len], false, nil
}

type Client struct {
	url string

	Debug        bool
	capabilities map[string]struct{}
	fetchCaps    map[string]struct{}
	lsRefsCaps   map[string]struct{}
}

func capsAsString(cps map[string]struct{}) string {
	var keys []string
	for k := range cps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func (cl *Client) String() string {
	return fmt.Sprintf("client{ URL %q, %s, fetch=[%s], ls-refs=[%s]",
		cl.url,
		capsAsString(cl.capabilities),
		capsAsString(cl.fetchCaps),
		capsAsString(cl.lsRefsCaps))
}

func (cl *Client) HasCap(cap string) bool {
	_, ok := cl.capabilities[cap]
	return ok
}

func NewClient(u string) (*Client, error) {
	suffix := "/info/refs?service=git-upload-pack"
	finalURL := u + suffix
	req, err := http.NewRequest("GET", finalURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Git-Protocol", "version=2")

	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			log.Printf("redirect to %s", req.URL)
			finalURL = req.URL.String()
			return nil
		},
	}
	rep, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer rep.Body.Close()
	s := newScanner(rep.Body)
	packet, magic, err := s.readLine()
	if err != nil {
		return nil, err
	}
	if packetString(packet) == "# service=git-upload-pack" {
		if packet, magic, err = s.readLine(); err != nil {
			return nil, err
		} else if !magic || !bytes.Equal(packet, flushPacket) {
			return nil, fmt.Errorf("want flush, got %q", packet)
		}

		if packet, _, err = s.readLine(); err != nil {
			return nil, err
		}
	}

	if packetString(packet) != "version 2" {
		return nil, fmt.Errorf("version 2 not supported for %s: %s", u, packet)
	}

	if !strings.HasSuffix(finalURL, suffix) {
		return nil, fmt.Errorf("suffix missing: %s", u)
	}
	finalURL = finalURL[:len(finalURL)-len(suffix)]

	if !strings.HasSuffix(finalURL, "/") {
		finalURL += "/"
	}
	cl := &Client{
		url:          finalURL + "git-upload-pack",
		capabilities: map[string]struct{}{},
		fetchCaps:    map[string]struct{}{},
		lsRefsCaps:   map[string]struct{}{},
	}

	for {
		packet, magic, err := s.readLine()
		if err == io.EOF || (magic && bytes.Equal(packet, flushPacket)) {
			break
		}
		if err != nil {
			return nil, err
		}

		str := packetString(packet)
		if strings.HasPrefix(str, "fetch=") {
			str = str[len("fetch="):]
			for _, c := range strings.Split(str, " ") {
				cl.fetchCaps[c] = struct{}{}
			}
			continue
		}
		if strings.HasPrefix(str, "ls-refs=") {
			str = str[len("ls-refs="):]
			for _, c := range strings.Split(str, " ") {
				cl.lsRefsCaps[c] = struct{}{}

			}
			continue
		}
		cl.capabilities[str] = struct{}{}
	}

	return cl, nil
}

type v2Req interface {
	Encode(w io.Writer) error
}

func (cl *Client) NewRequest(v2req v2Req) (*http.Request, error) {
	buf := &bytes.Buffer{}
	if err := v2req.Encode(buf); err != nil {
		return nil, fmt.Errorf("encode: %v", err)
	}
	req, err := http.NewRequest("POST", cl.url, buf)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Git-Protocol", "version=2")
	req.Header.Add(
		"content-type", "application/x-git-upload-pack-request")
	req.Header.Add(
		"accept", "application/x-git-upload-pack-result")

	return req, nil
}

func (cl *Client) do(req *http.Request) (*http.Response, error) {
	if cl.Debug {
		log.Printf("Req %v", req)
	}
	rep, err := http.DefaultClient.Do(req)
	if cl.Debug {
		if err != nil {
			log.Printf("HTTP err: %v", err)
		} else {
			log.Printf("HTTP rep: %v", rep)
		}
	}
	return rep, err
}

func (cl *Client) ObjectInfo(ids []plumbing.Hash) (map[plumbing.Hash]uint64, error) {
	if len(ids) == 0 {
		return map[plumbing.Hash]uint64{}, nil
	}
	if _, ok := cl.capabilities["object-info"]; !ok {
		return nil, ErrUnsupported
	}
	oiReq := &ObjectInfoRequest{
		Want: "size",
		OID:  ids,
	}

	req, err := cl.NewRequest(oiReq)
	if err != nil {
		return nil, err
	}
	rep, err := cl.do(req)
	if err != nil {
		return nil, err
	}
	defer rep.Body.Close()

	oiRep := &ObjectInfoResponse{}

	if err := oiRep.Decode(rep.Body); err != nil {
		return nil, err
	}

	result := make(map[plumbing.Hash]uint64)
	for _, e := range oiRep.Sizes {
		result[e.Hash] = e.Size
	}
	return result, nil
}

type FetchRequest struct {
	Want           []plumbing.Hash
	Have           []plumbing.Hash
	Shallow        []plumbing.Hash
	Deepen         *int
	DeepenRelative bool
	DeepenNot      plumbing.Hash
	// DeepenSince
	Done         bool
	ThinPack     bool
	NoProgress   bool
	IncludeTag   bool
	OFSDelta     bool
	Filter       string
	WantRef      []string
	SidebandAll  bool
	PackfileURIs []string
	WaitForDone  bool
}

func (r *FetchRequest) Encode(w io.Writer) error {
	e := pktline.NewEncoder(w)
	e.EncodeString("command=fetch\n")
	w.Write([]byte("0001"))
	for _, w := range r.Want {
		e.Encodef("want %s\n", w)
	}
	for _, id := range r.Have {
		e.Encodef("have %s\n", id)
	}
	for _, id := range r.WantRef {
		e.Encodef("want-ref %s\n", id)
	}
	for _, id := range r.Shallow {
		e.Encodef("shallow %s\n", id)
	}

	if r.Deepen != nil {
		e.Encodef("deepen %d\n", *r.Deepen)
	}
	if r.DeepenNot != plumbing.ZeroHash {
		e.Encodef("deepen-not %s\n", r.DeepenNot)
	}

	for k, v := range map[string]bool{
		"done":            r.Done,
		"thin-pack":       r.ThinPack,
		"no-progress":     r.NoProgress,
		"include-tag":     r.IncludeTag,
		"ofs-delta":       r.OFSDelta,
		"sideband-all":    r.SidebandAll,
		"wait-for-done":   r.WaitForDone,
		"deepen-relative": r.DeepenRelative,
	} {
		if v {
			e.Encodef("%s\n", k)
		}
	}
	if r.Filter != "" {
		e.Encodef("filter %s\n", r.Filter)
	}
	if len(r.PackfileURIs) > 0 {
		e.Encodef("packfile-uris %s\n", strings.Join(r.PackfileURIs, ","))
	}
	e.Flush()
	return nil
}

type FetchOptions struct {
	Progress io.Writer
	Want     []plumbing.Hash
	Depth    int
	Filter   string
}

func (cl *Client) Fetch(storer storage.Storer, opts *FetchOptions) error {
	start := time.Now()
	fetchReq := &FetchRequest{
		Done:   true,
		Want:   opts.Want,
		Filter: opts.Filter,
	}
	if opts.Depth > 0 {
		fetchReq.Deepen = &opts.Depth
	}

	if opts.Progress == nil {
		opts.Progress = io.Discard
	}
	req, err := cl.NewRequest(fetchReq)
	if err != nil {
		return err
	}
	rep, err := cl.do(req)
	if err != nil {
		return err
	}
	if rep.StatusCode != 200 {
		return fmt.Errorf("status %s", rep.Status)
	}
	defer rep.Body.Close()

	sc := newScanner(rep.Body)
	pack := &bytes.Buffer{}

	errorBuf := &bytes.Buffer{}
responseLoop:
	for {
		packet, magic, err := sc.readLine()
		if magic {
			break
		}
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			return err
		}

		switch packetString(packet) {

		case "shallow-info":
			for {
				p, magic, err := sc.readLine()
				if err != nil {
					return err
				}
				if magic && bytes.Equal(delimiterPacket, p) {
					break
				}
			}
		case "packfile":
			for {
				p, magic, err := sc.readLine()
				if (magic && bytes.Equal(flushPacket, p)) || err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				typ := p[0]
				p = p[1:]
				switch typ {
				case 1:
					pack.Write(p)
				case 2:
					fmt.Fprintf(opts.Progress, "%s: %s", cl.url, string(p))
				case 3:
					errorBuf.Write(p)
				default:
					return fmt.Errorf("unknown channel %d", typ)
				}
				if bytes.Equal(delimiterPacket, p) {
					break
				}
			}
			break responseLoop
		}
	}

	if errorBuf.Len() > 0 {
		return errors.New(errorBuf.String())
	}

	log.Printf("%s: fetch took %s", cl.url, time.Now().Sub(start))
	return packfile.UpdateObjectStorage(storer, pack)
}
