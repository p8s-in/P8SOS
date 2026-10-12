package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Razorpay is a small client for Razorpay's Orders and Payments APIs.
type Razorpay struct {
	KeyID, KeySecret string
	Base             string       // default https://api.razorpay.com/v1
	HTTP             *http.Client // default 30 s timeout
}

func (r *Razorpay) do(ctx context.Context, method, path string, body, out any) error {
	base := r.Base
	if base == "" {
		base = "https://api.razorpay.com/v1"
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.KeyID, r.KeySecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("razorpay: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Description string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error.Description == "" {
			e.Error.Description = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("razorpay (HTTP %d): %s", resp.StatusCode, e.Error.Description)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// CreateOrder makes an order for amount (smallest unit) and returns its id.
func (r *Razorpay) CreateOrder(ctx context.Context, amount int64, currency, receipt string, notes map[string]string) (string, error) {
	var o struct {
		ID string `json:"id"`
	}
	err := r.do(ctx, "POST", "/orders", map[string]any{"amount": amount, "currency": currency, "receipt": receipt, "notes": notes}, &o)
	if err == nil && o.ID == "" {
		err = fmt.Errorf("razorpay returned no order id")
	}
	return o.ID, err
}

// Payment is what P8S needs to know about a payment.
type Payment struct {
	ID       string `json:"id"`
	OrderID  string `json:"order_id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"` // created, authorized, captured, refunded, failed
}

// FetchPayment reads a payment.
func (r *Razorpay) FetchPayment(ctx context.Context, id string) (Payment, error) {
	var p Payment
	err := r.do(ctx, "GET", "/payments/"+id, nil, &p)
	return p, err
}

// Capture captures an authorized payment (when auto-capture is off).
func (r *Razorpay) Capture(ctx context.Context, id string, amount int64, currency string) (Payment, error) {
	var p Payment
	err := r.do(ctx, "POST", "/payments/"+id+"/capture", map[string]any{"amount": amount, "currency": currency}, &p)
	return p, err
}

func mac(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckoutSignature verifies the signature Checkout hands the browser:
// HMAC-SHA256(order_id + "|" + payment_id, key secret).
func CheckoutSignature(orderID, paymentID, signature, keySecret string) bool {
	return keySecret != "" && hmac.Equal([]byte(mac(keySecret, orderID+"|"+paymentID)), []byte(signature))
}

// WebhookSignature verifies X-Razorpay-Signature over the raw request body.
func WebhookSignature(body []byte, signature, webhookSecret string) bool {
	return webhookSecret != "" && hmac.Equal([]byte(mac(webhookSecret, string(body))), []byte(signature))
}
