package objinfo

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
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

// go-git pktline doesn't do protocol v2.

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
var errShortRead = fmt.Errorf("short read")

func (s *scanner) readString() (string, []byte, error) {
	l, err := s.readLine()
	str := string(l)
	if str != "" && str[len(str)-1] == '\n' {
		str = str[:len(str)-1]
	}

	return str, l, err
}

func (s *scanner) readLine() ([]byte, error) {
	if n, err := s.r.Read(s.buf[:4]); err != nil {
		return nil, err
	} else if n != 4 {
		return nil, errShortRead
	}

	if string(s.buf[:4]) == "0000" {
		return flushPacket, nil
	}
	if string(s.buf[:4]) == "0001" {
		return delimiterPacket, nil
	}

	var len int
	_, err := fmt.Sscanf(string(s.buf[:4]), "%x", &len)
	if err != nil {
		return nil, err
	}

	len -= 4
	if cap(s.buf) < int(len) {
		s.buf = make([]byte, len)
	}
	if n, err := s.r.Read(s.buf[:len]); err != nil {
		return nil, err
	} else if n != int(len) {
		return nil, errShortRead
	}
	return s.buf[:len], nil
}

func ObjectInfo(u *url.URL, ids []plumbing.Hash) (map[plumbing.Hash]uint64, error) {
	suffix := "/info/refs?service=git-upload-pack"
	req, err := http.NewRequest("GET", u.String()+suffix, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Git-Protocol", "version=2")

	finalURL := u.String()
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
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
	line, _, err := s.readString()
	if err != nil {
		return nil, err
	}
	if line != "# service=git-upload-pack" {
		return nil, fmt.Errorf("missing service line: %q", line)
	}

	if packet, err := s.readLine(); err != nil {
		return nil, err
	} else if !bytes.Equal(packet, flushPacket) {
		return nil, fmt.Errorf("want flush, got %q", packet)
	}

	if str, _, err := s.readString(); err != nil {
		return nil, err
	} else if str != "version 2" {
		return nil, fmt.Errorf("version 2 not supported: %s", line)
	}

	found := false
	for {
		str, packet, err := s.readString()
		if err != nil {
			return nil, err
		}
		if bytes.Equal(packet, flushPacket) {
			break
		}
		if str == "object-info" {
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("object-info capability not found")
	}

	if !strings.HasSuffix(finalURL, suffix) {
		return nil, fmt.Errorf("suffix missing: %s", u)
	}
	finalURL = finalURL[:len(finalURL)-len(suffix)]
	oiReq := ObjectInfoRequest{
		Want: "size",
		OID:  ids,
	}

	buf := &bytes.Buffer{}
	if err := oiReq.Encode(buf); err != nil {
		return nil, fmt.Errorf("encode: %v", err)
	}
	req, err = http.NewRequest("POST", finalURL+"/git-upload-pack", buf)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Git-Protocol", "version=2")
	req.Header.Add(
		"content-type", "application/x-git-upload-pack-request")
	req.Header.Add(
		"accept", "application/x-git-upload-pack-result")

	rep, err = client.Do(req)
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
	log.Println(result)
	return result, nil
}
