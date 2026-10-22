package obj

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sigAlg        = "AWS4-HMAC-SHA256"
	unsignedBody  = "UNSIGNED-PAYLOAD"
	amzDateFormat = "20060102T150405Z"
)

type signer struct {
	key, secret, region string
}

func (g signer) scope(now time.Time) string {
	return now.UTC().Format("20060102") + "/" + g.region + "/s3/aws4_request"
}

func hmacSHA256(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func (g signer) signature(now time.Time, canonical string) string {
	k := hmacSHA256([]byte("AWS4"+g.secret), now.UTC().Format("20060102"))
	k = hmacSHA256(k, g.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sum := sha256.Sum256([]byte(canonical))
	toSign := sigAlg + "\n" + now.UTC().Format(amzDateFormat) + "\n" + g.scope(now) + "\n" + hex.EncodeToString(sum[:])
	return hex.EncodeToString(hmacSHA256(k, toSign))
}

func uriEncode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/' && !slash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func canonicalRequest(method, escPath, query string, headers map[string]string, payloadHash string) (canonical, signed string) {
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var h strings.Builder
	for _, n := range names {
		h.WriteString(n + ":" + strings.TrimSpace(headers[n]) + "\n")
	}
	signed = strings.Join(names, ";")
	return method + "\n" + escPath + "\n" + query + "\n" + h.String() + "\n" + signed + "\n" + payloadHash, signed
}

// sign sets x-amz-date, x-amz-content-sha256 and Authorization on req, whose
// RawQuery must already be canonical.
func (g signer) sign(req *http.Request, payloadHash string, now time.Time) {
	req.Header.Set("X-Amz-Date", now.UTC().Format(amzDateFormat))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k, vs := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(vs, ",")
	}
	canonical, signed := canonicalRequest(req.Method, req.URL.EscapedPath(), req.URL.RawQuery, headers, payloadHash)
	req.Header.Set("Authorization", sigAlg+" Credential="+g.key+"/"+g.scope(now)+", SignedHeaders="+signed+", Signature="+g.signature(now, canonical))
}

func (g signer) presignGet(host, escPath string, now time.Time, ttl time.Duration) string {
	q := url.Values{
		"X-Amz-Algorithm":     {sigAlg},
		"X-Amz-Credential":    {g.key + "/" + g.scope(now)},
		"X-Amz-Date":          {now.UTC().Format(amzDateFormat)},
		"X-Amz-Expires":       {strconv.FormatInt(int64(ttl/time.Second), 10)},
		"X-Amz-SignedHeaders": {"host"},
	}
	cq := canonicalQuery(q)
	canonical, _ := canonicalRequest("GET", escPath, cq, map[string]string{"host": host}, unsignedBody)
	return escPath + "?" + cq + "&X-Amz-Signature=" + g.signature(now, canonical)
}
