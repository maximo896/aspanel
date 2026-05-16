package interact

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	interactshclient "github.com/projectdiscovery/interactsh/pkg/client"
	interactshserver "github.com/projectdiscovery/interactsh/pkg/server"
)

type Client struct {
	Timeout time.Duration
}

func NewClient(command string) *Client {
	return &Client{
		Timeout: 8 * time.Second,
	}
}

var (
	initOnce    sync.Once
	initErr     error
	sdk         *interactshclient.Client
	payloadURL  string
	eventQueue  = make(chan Signal, 4096)
	queueMaxAge = 30 * time.Minute
)

func ensureStarted() error {
	initOnce.Do(func() {
		sdk, initErr = interactshclient.New(&interactshclient.Options{
			ServerURL: "oast.pro,oast.live,oast.site,oast.online,oast.fun,oast.me",
		})
		if initErr != nil {
			return
		}
		payloadURL = strings.TrimSpace(sdk.URL())
		if payloadURL == "" {
			initErr = fmt.Errorf("interactsh payload url is empty")
			return
		}
		if err := sdk.StartPolling(5*time.Second, func(interaction *interactshserver.Interaction) {
			sig, ok := parseInteraction(interaction)
			if !ok {
				return
			}
			select {
			case eventQueue <- sig:
			default:
			}
		}); err != nil {
			initErr = fmt.Errorf("interactsh start polling failed: %w", err)
			return
		}
	})
	return initErr
}

func CallbackURL() (string, error) {
	if err := ensureStarted(); err != nil {
		return "", err
	}
	u := strings.TrimSpace(payloadURL)
	if u == "" {
		return "", fmt.Errorf("interactsh payload url is empty")
	}
	if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
		u = "https://" + u
	}
	return u, nil
}

func (c *Client) Fetch() ([]Signal, error) {
	if err := ensureStarted(); err != nil {
		return nil, err
	}
	out := make([]Signal, 0, 16)
	for {
		select {
		case sig := <-eventQueue:
			if time.Since(sig.At) > queueMaxAge {
				continue
			}
			out = append(out, sig)
		default:
			return out, nil
		}
	}
}

func parseInteraction(interaction *interactshserver.Interaction) (Signal, bool) {
	if interaction == nil {
		return Signal{}, false
	}
	raw := strings.TrimSpace(interaction.RawRequest)
	if raw == "" {
		return Signal{}, false
	}
	firstLine := strings.SplitN(raw, "\n", 2)[0]
	parts := strings.Split(firstLine, " ")
	if len(parts) < 2 {
		return Signal{}, false
	}
	u, err := url.Parse(parts[1])
	if err != nil {
		return Signal{}, false
	}
	q := u.Query()
	token := strings.TrimSpace(q.Get("token"))
	kind := strings.TrimSpace(q.Get("kind"))
	region := strings.TrimSpace(q.Get("region"))
	zone := strings.TrimSpace(q.Get("zone"))
	proto := strings.TrimSpace(q.Get("proto"))
	if token == "" || proto == "" {
		return Signal{}, false
	}
	if !strings.HasPrefix(proto, "awvsagent://") &&
		!strings.HasPrefix(proto, "sqlmapagent://") &&
		!strings.HasPrefix(proto, "pathagent://") &&
		!strings.HasPrefix(proto, "bootstrap://") {
		return Signal{}, false
	}
	return Signal{
		Token:  token,
		Proto: proto,
		Region: func() string {
			if region != "" {
				return region
			}
			return ""
		}(),
		Zone: zone,
		Kind: kind,
		At:   time.Now(),
	}, true
}
