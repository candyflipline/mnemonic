package obj

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const deleteBatch = 1000

type s3Store struct {
	base   url.URL
	bucket string
	prefix string
	sg     signer
	hc     *http.Client
}

func openS3(u *url.URL) (*s3Store, error) {
	q := u.Query()
	endpoint, region := q.Get("endpoint"), q.Get("region")
	if u.Host == "" || endpoint == "" || region == "" {
		return nil, fmt.Errorf("obj: s3 uri needs s3://bucket/prefix?endpoint=HOST&region=R")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	eu, err := url.Parse(endpoint)
	if err != nil || eu.Host == "" {
		return nil, fmt.Errorf("obj: endpoint %q is not a host", q.Get("endpoint"))
	}
	id, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if id == "" || secret == "" {
		return nil, fmt.Errorf("obj: s3 needs AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in the env")
	}
	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &s3Store{
		base:   url.URL{Scheme: eu.Scheme, Host: eu.Host},
		bucket: u.Host,
		prefix: prefix,
		sg:     signer{key: id, secret: secret, region: region},
		hc: &http.Client{Transport: &http.Transport{
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}},
	}, nil
}

func (s *s3Store) objPath(k Key) string { return "/" + s.bucket + "/" + s.prefix + string(k) }

func (s *s3Store) Put(ctx context.Context, k Key, r io.Reader, size int64) error {
	return s.put(ctx, k, r, size, false)
}

func (s *s3Store) PutExclusive(ctx context.Context, k Key, r io.Reader, size int64) error {
	return s.put(ctx, k, r, size, true)
}

func (s *s3Store) put(ctx context.Context, k Key, r io.Reader, size int64, exclusive bool) error {
	if err := checkKey(k, false); err != nil {
		return err
	}
	body, hash, err := hashBody(r, size)
	if err != nil {
		return err
	}
	hdr := http.Header{}
	if exclusive {
		hdr.Set("If-None-Match", "*")
	}
	resp, err := s.do(ctx, "PUT", s.objPath(k), nil, hdr, body, size, hash)
	if err != nil {
		return err
	}
	defer drain(resp)
	if exclusive && resp.StatusCode == http.StatusPreconditionFailed {
		return ErrExists
	}
	return s3Err(resp, "PUT "+string(k))
}

func hashBody(r io.Reader, size int64) (io.Reader, string, error) {
	h := sha256.New()
	if rs, ok := r.(io.ReadSeeker); ok {
		pos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, "", err
		}
		if n, err := io.Copy(h, io.LimitReader(rs, size)); err != nil || n != size {
			return nil, "", fmt.Errorf("obj: reader gave %d of %d bytes: %w", n, size, orEOF(err))
		}
		if _, err := rs.Seek(pos, io.SeekStart); err != nil {
			return nil, "", err
		}
		return io.LimitReader(rs, size), hex.EncodeToString(h.Sum(nil)), nil
	}
	var buf bytes.Buffer
	if n, err := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(r, size)); err != nil || n != size {
		return nil, "", fmt.Errorf("obj: reader gave %d of %d bytes: %w", n, size, orEOF(err))
	}
	return &buf, hex.EncodeToString(h.Sum(nil)), nil
}

func orEOF(err error) error {
	if err == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}

type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

func (s *s3Store) List(ctx context.Context, prefix Key) ([]Entry, error) {
	if err := checkKey(prefix, true); err != nil {
		return nil, err
	}
	var out []Entry
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {s.prefix + string(prefix)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, "GET", "/"+s.bucket, q, nil, nil, 0, hashEmpty)
		if err != nil {
			return nil, err
		}
		var res listResult
		err = s3Err(resp, "LIST "+string(prefix))
		if err == nil {
			err = xml.NewDecoder(resp.Body).Decode(&res)
		}
		drain(resp)
		if err != nil {
			return nil, err
		}
		for _, c := range res.Contents {
			out = append(out, Entry{Key: Key(strings.TrimPrefix(c.Key, s.prefix)), Size: c.Size})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

func (s *s3Store) Delete(ctx context.Context, keys []Key) error {
	for _, k := range keys {
		if err := checkKey(k, false); err != nil {
			return err
		}
	}
	for len(keys) > 0 {
		n := min(len(keys), deleteBatch)
		var b bytes.Buffer
		b.WriteString("<Delete><Quiet>true</Quiet>")
		for _, k := range keys[:n] {
			b.WriteString("<Object><Key>")
			xml.EscapeText(&b, []byte(s.prefix+string(k)))
			b.WriteString("</Key></Object>")
		}
		b.WriteString("</Delete>")
		keys = keys[n:]
		sum := md5.Sum(b.Bytes())
		hdr := http.Header{}
		hdr.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
		hash := sha256.Sum256(b.Bytes())
		size := int64(b.Len())
		resp, err := s.do(ctx, "POST", "/"+s.bucket, url.Values{"delete": {""}}, hdr, &b, size, hex.EncodeToString(hash[:]))
		if err != nil {
			return err
		}
		var res struct {
			Errors []struct{ Key, Code, Message string } `xml:"Error"`
		}
		err = s3Err(resp, "DELETE")
		if err == nil {
			xml.NewDecoder(resp.Body).Decode(&res)
			if len(res.Errors) > 0 {
				e := res.Errors[0]
				err = fmt.Errorf("obj: s3 DELETE %s: %s %s", e.Key, e.Code, e.Message)
			}
		}
		drain(resp)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *s3Store) PresignGetUntil(k Key, notAfter time.Time) (string, error) {
	if err := checkKey(k, false); err != nil {
		return "", err
	}
	now := time.Now()
	ttl := notAfter.Sub(now).Truncate(time.Second)
	if ttl < time.Second || ttl > 7*24*time.Hour {
		return "", fmt.Errorf("obj: presign ttl %s is outside 1s..7d", ttl)
	}
	esc := uriEncode(s.objPath(k), false)
	return s.base.Scheme + "://" + s.base.Host + s.sg.presignGet(s.base.Host, esc, now, ttl), nil
}

const hashEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *s3Store) do(ctx context.Context, method, path string, q url.Values, hdr http.Header, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	raw := s.base.Scheme + "://" + s.base.Host + uriEncode(path, false)
	if len(q) > 0 {
		raw += "?" + canonicalQuery(q)
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	req.ContentLength = size
	if body != nil && size == 0 {
		req.Body = http.NoBody
	}
	s.sg.sign(req, payloadHash, time.Now())
	return s.hc.Do(req)
}

func s3Err(resp *http.Response, what string) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct{ Code, Message string }
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	xml.Unmarshal(b, &e)
	return fmt.Errorf("obj: s3 %s: %d %s %s", what, resp.StatusCode, e.Code, e.Message)
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}
