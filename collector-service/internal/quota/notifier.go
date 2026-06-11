package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// CrossingAlert is one quota-threshold crossing worth emailing about. The
// materializer builds it the moment a notify-enabled threshold is newly crossed
// (deduped via triggered_thresholds, so at most once per period) and hands it to
// an AlertNotifier. NotifyEmail is forwarded verbatim — "" means the seat's own
// email, resolved control-side (the collector has no org_seats email).
type CrossingAlert struct {
	SubjectID   string  `json:"subject_id"`
	SubjectKind string  `json:"subject_kind"`
	Metric      string  `json:"metric"`
	Period      string  `json:"period"`
	Pct         int     `json:"pct"`
	Used        float64 `json:"used"`
	Limit       float64 `json:"limit"`
	NotifyEmail string  `json:"notify_email"`
}

// AlertNotifier delivers a crossing alert out-of-band. NotifyCrossing must not
// block the projection path (the materializer runs inside DWD projection), so
// implementations fire-and-forget.
type AlertNotifier interface {
	NotifyCrossing(a CrossingAlert)
}

// HTTPNotifier POSTs crossings to control's /internal/quota-alert (architecture
// B: collector detects, control owns SMTP + recipient resolution). Service-token
// authenticated, exactly like the proxy's resolve-token call.
type HTTPNotifier struct {
	url    string // control base URL, e.g. http://control-service:8080
	token  string // SERVICE_TOKEN bearer
	client *http.Client
	logger *slog.Logger
}

// NewHTTPNotifier returns a notifier, or nil if controlURL/token are unset so the
// materializer simply skips notification (e.g. editions without a control plane).
func NewHTTPNotifier(controlURL, token string, logger *slog.Logger) *HTTPNotifier {
	if controlURL == "" || token == "" {
		return nil
	}
	return &HTTPNotifier{
		url:    controlURL,
		token:  token,
		client: &http.Client{Timeout: 8 * time.Second},
		logger: logger,
	}
}

func (n *HTTPNotifier) NotifyCrossing(a CrossingAlert) {
	// Fire-and-forget: a slow/unavailable control plane must never stall DWD
	// projection. Crossings are rare (deduped per period) so unbounded goroutines
	// aren't a concern in practice.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		body, err := json.Marshal(a)
		if err != nil {
			n.logger.Warn("quota.alert.marshal_failed", "error", err.Error())
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url+"/internal/quota-alert", bytes.NewReader(body))
		if err != nil {
			n.logger.Warn("quota.alert.request_failed", "error", err.Error())
			return
		}
		req.Header.Set("Authorization", "Bearer "+n.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.client.Do(req)
		if err != nil {
			n.logger.Warn("quota.alert.post_failed", "subject_id", a.SubjectID, "error", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			n.logger.Warn("quota.alert.post_non2xx", "subject_id", a.SubjectID, "status", resp.StatusCode)
		}
	}()
}
