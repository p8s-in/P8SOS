package offsite

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// s3Backend talks to S3-compatible storage (AWS S3, Backblaze B2, Cloudflare
// R2, Wasabi, MinIO, …) with Signature V4. Uploads are always multipart, so
// any size streams without knowing it in advance.
type s3Backend struct {
	endpoint  *url.URL // https://s3.ap-south-1.amazonaws.com
	region    string   // ap-south-1; R2: auto
	bucket    string
	access    string
	secret    string
	pathStyle bool // endpoint/bucket/key (MinIO, B2, R2) instead of bucket.endpoint/key
	partSize  int
	client    *http.Client
	now       func() time.Time
}

const unsignedPayload = "UNSIGNED-PAYLOAD"

func newS3(t Target, client *http.Client) (*s3Backend, error) {
	u, err := url.Parse(strings.TrimRight(t.Endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("endpoint must be a URL like https://s3.ap-south-1.amazonaws.com")
	}
	region := t.Region
	if region == "" {
		region = "us-east-1"
	}
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return &s3Backend{endpoint: u, region: region, bucket: t.Bucket, access: t.AccessKey, secret: t.SecretKey,
		pathStyle: !t.VirtualHost, partSize: 64 << 20, client: client, now: time.Now}, nil
}

// awsEscape encodes like AWS wants: unreserved characters stay, "/" stays in paths.
func awsEscape(s string, path bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && path:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

// request builds and signs a request for key (may be "" for the bucket).
func (s *s3Backend) request(ctx context.Context, method, key string, q url.Values, body io.Reader, payloadHash string) (*http.Request, error) {
	host := s.endpoint.Host
	path := "/" + key
	if s.pathStyle {
		path = "/" + s.bucket + "/" + key
		if key == "" {
			path = "/" + s.bucket
		}
	} else {
		host = s.bucket + "." + host
	}
	escPath := awsEscape(path, true)
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		qs = append(qs, awsEscape(k, false)+"="+awsEscape(q.Get(k), false))
	}
	query := strings.Join(qs, "&")
	u := s.endpoint.Scheme + "://" + host + escPath
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	signed := "host;x-amz-content-sha256;x-amz-date"
	canon := strings.Join([]string{method, escPath, query,
		"host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n",
		signed, payloadHash}, "\n")
	scope := day + "/" + s.region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canon))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA([]byte("AWS4"+s.secret), day)
	k = hmacSHA(k, s.region)
	k = hmacSHA(k, "s3")
	k = hmacSHA(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", s.access, scope, signed, sig))
	req.Host = host
	return req, nil
}

type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (s *s3Backend) do(req *http.Request, want ...int) (*http.Response, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	for _, w := range want {
		if resp.StatusCode == w {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e s3Error
	_ = xml.Unmarshal(b, &e)
	if resp.StatusCode == http.StatusNotFound && (e.Code == "NoSuchKey" || e.Code == "") {
		return nil, ErrNotFound
	}
	if e.Code != "" {
		return nil, fmt.Errorf("storage said %s: %s (HTTP %d)", e.Code, e.Message, resp.StatusCode)
	}
	return nil, fmt.Errorf("storage answered HTTP %d", resp.StatusCode)
}

func (s *s3Backend) Put(ctx context.Context, key string, r io.Reader) error {
	if err := safeKey(key); err != nil {
		return err
	}
	req, err := s.request(ctx, "POST", key, url.Values{"uploads": {""}}, nil, "")
	if err != nil {
		return err
	}
	resp, err := s.do(req, 200)
	if err != nil {
		return fmt.Errorf("start upload: %w", err)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadID == "" {
		return fmt.Errorf("start upload: no upload id")
	}
	abort := func() {
		if req, err := s.request(context.WithoutCancel(ctx), "DELETE", key, url.Values{"uploadId": {init.UploadID}}, nil, ""); err == nil {
			if resp, err := s.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []part
	buf := make([]byte, s.partSize)
	for n := 1; ; n++ {
		m, rerr := io.ReadFull(r, buf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			abort()
			return rerr
		}
		if m == 0 && n > 1 {
			break
		}
		if n > 10000 {
			abort()
			return errors.New("file too large for this storage (10,000 parts)")
		}
		var etag string
		for try := 0; ; try++ { // a part can be sent again
			req, err := s.request(ctx, "PUT", key, url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {init.UploadID}}, bytes.NewReader(buf[:m]), "")
			if err != nil {
				abort()
				return err
			}
			req.ContentLength = int64(m)
			resp, err := s.do(req, 200)
			if err == nil {
				etag = resp.Header.Get("ETag")
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				break
			}
			if try == 2 || ctx.Err() != nil {
				abort()
				return fmt.Errorf("upload part %d: %w", n, err)
			}
			time.Sleep(time.Duration(try+1) * 2 * time.Second)
		}
		parts = append(parts, part{n, etag})
		if m < len(buf) {
			break
		}
	}
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&body, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.Number, xmlEscape(p.ETag))
	}
	body.WriteString("</CompleteMultipartUpload>")
	sum := sha256.Sum256(body.Bytes())
	req, err = s.request(ctx, "POST", key, url.Values{"uploadId": {init.UploadID}}, bytes.NewReader(body.Bytes()), hex.EncodeToString(sum[:]))
	if err != nil {
		abort()
		return err
	}
	req.ContentLength = int64(body.Len())
	resp, err = s.do(req, 200)
	if err != nil {
		abort()
		return fmt.Errorf("finish upload: %w", err)
	}
	// S3 can answer 200 with an error inside
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var e s3Error
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		abort()
		return fmt.Errorf("finish upload: %s: %s", e.Code, e.Message)
	}
	return nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *s3Backend) Get(ctx context.Context, key string, w io.Writer) error {
	if err := safeKey(key); err != nil {
		return err
	}
	req, err := s.request(ctx, "GET", key, nil, nil, "")
	if err != nil {
		return err
	}
	resp, err := s.do(req, 200)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (s *s3Backend) List(ctx context.Context, prefix string) ([]Object, error) {
	out := []Object{}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := s.request(ctx, "GET", "", q, nil, "")
		if err != nil {
			return nil, err
		}
		resp, err := s.do(req, 200)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("bucket %q not found", s.bucket)
			}
			return nil, err
		}
		var l struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&l)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}
		for _, c := range l.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size})
		}
		if !l.IsTruncated || l.Next == "" {
			return out, nil
		}
		token = l.Next
	}
}

func (s *s3Backend) Delete(ctx context.Context, key string) error {
	if err := safeKey(key); err != nil {
		return err
	}
	req, err := s.request(ctx, "DELETE", key, nil, nil, "")
	if err != nil {
		return err
	}
	resp, err := s.do(req, 200, 204)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
