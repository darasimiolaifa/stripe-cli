package logtailing

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/ansi"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const outputFormatJSON = "JSON"

// LogFilters contains all of the potential user-provided filters for log tailing
type LogFilters struct {
	FilterAccount        []string `json:"filter_account,omitempty"`
	FilterIPAddress      []string `json:"filter_ip_address,omitempty"`
	FilterHTTPMethod     []string `json:"filter_http_method,omitempty"`
	FilterRequestPath    []string `json:"filter_request_path,omitempty"`
	FilterRequestStatus  []string `json:"filter_request_status,omitempty"`
	FilterSource         []string `json:"filter_source,omitempty"`
	FilterStatusCode     []string `json:"filter_status_code,omitempty"`
	FilterStatusCodeType []string `json:"filter_status_code_type,omitempty"`
}

// Config provides the configuration of a log tailer
type Config struct {
	APIBaseURL string

	// DeviceName is the name of the device sent to Stripe to help identify the device
	DeviceName string

	// Filters for API request logs
	Filters *LogFilters

	// Key is the API key used to authenticate with Stripe
	Key string

	// Info, error, etc. logger. Unrelated to API request logs.
	Log *log.Logger

	// Force use of unencrypted ws:// protocol instead of wss://
	NoWSS bool

	// Output format for request logs
	OutputFormat string

	// WebSocketFeature is the feature specified for the websocket connection
	WebSocketFeature string
}

// Tailer is the main interface for running the log tailing session
type Tailer struct {
	cfg *Config

	interruptCh chan os.Signal
}

// EventPayload is the mapping for fields in event payloads from request log tailing
type EventPayload struct {
	CreatedAt int           `json:"created_at"`
	Livemode  bool          `json:"livemode"`
	Method    string        `json:"method"`
	RequestID string        `json:"request_id"`
	Status    int           `json:"status"`
	URL       string        `json:"url"`
	Error     RedactedError `json:"error"`
}

// RedactedError is the mapping for fields in error from an EventPayload
type RedactedError struct {
	Type        string `json:"type"`
	Charge      string `json:"charge"`
	Code        string `json:"code"`
	DeclineCode string `json:"decline_code"`
	Message     string `json:"message"`
	Param       string `json:"param"`
}

// New creates a new Tailer
func New(cfg *Config) *Tailer {
	if cfg.Log == nil {
		cfg.Log = &log.Logger{Out: ioutil.Discard}
	}

	return &Tailer{
		cfg:         cfg,
		interruptCh: make(chan os.Signal, 1),
	}
}

func withSIGTERMCancel(ctx context.Context, onCancel func()) (context.Context, func()) {
	// Create a context that will be canceled when Ctrl+C is pressed
	ctx, cancel := context.WithCancel(ctx)

	interruptCh := make(chan os.Signal, 1)
	signal.Notify(interruptCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-interruptCh
		onCancel()
		cancel()
	}()
	return ctx, cancel
}

// Run sets the websocket connection
func (t *Tailer) Run(ctx context.Context) error {
	ansi.StartNewSpinner("Getting ready...", t.cfg.Log.Out)

	ctx, cancel := withSIGTERMCancel(ctx, func() {
		log.WithFields(log.Fields{
			"prefix": "logtailing.Tailer.Run",
		}).Debug("Ctrl+C received, cleaning up...")
	})

	connectionManager := websocket.NewConnectionManager(&websocket.ConnectionManagerCfg{
		NoWSS:            t.cfg.NoWSS,
		Logger:           t.cfg.Log,
		DeviceName:       t.cfg.DeviceName,
		WebSocketFeature: t.cfg.WebSocketFeature,
		PongWait:         10 * time.Second,
		WriteWait:        5 * time.Second,
	})

	// TODO: replace that with a ConnectionManager
	// client := websocket.NewClient(
	// 	&websocket.Config{
	// 		Log:   t.cfg.Log,
	// 		NoWSS: t.cfg.NoWSS,

	// 		GetReconnectInterval: func(session *stripeauth.StripeCLISession) time.Duration {
	// 			return time.Duration(session.ReconnectDelay) * time.Second
	// 		},
	// 	},
	// )

	onMessage := func(b []byte) {
		var msg websocket.IncomingMessage
		err := json.Unmarshal(b, &msg)
		if err != nil {
			t.cfg.Log.Debug("Received malformed message: ", err)
		}
		t.processRequestLogEvent(msg)
	}

	errorCh := make(chan error)
	onTerminate := func(err error) {
		t.cfg.Log.Fatal("Terminating...", err)
		cancel()
		errorCh <- err
	}

	connectionManager.Run(ctx, onMessage, onTerminate)
	select {
	case err := <-errorCh:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (t *Tailer) processRequestLogEvent(msg websocket.IncomingMessage) {
	if msg.RequestLogEvent == nil {
		t.cfg.Log.Debug("WebSocket specified for request logs received non-request-logs event")
		return
	}

	requestLogEvent := msg.RequestLogEvent

	t.cfg.Log.WithFields(log.Fields{
		"prefix":     "logtailing.Tailer.processRequestLogEvent",
		"webhook_id": requestLogEvent.RequestLogID,
	}).Debugf("Processing request log event")

	var payload EventPayload
	if err := json.Unmarshal([]byte(requestLogEvent.EventPayload), &payload); err != nil {
		t.cfg.Log.Debug("Received malformed payload: ", err)
	}

	// Don't show stripecli/sessions logs since they're generated by the CLI
	if payload.URL == "/v1/stripecli/sessions" {
		t.cfg.Log.Debug("Filtering out /v1/stripecli/sessions from logs")
		return
	}

	if t.cfg.OutputFormat == outputFormatJSON {
		fmt.Println(ansi.ColorizeJSON(requestLogEvent.EventPayload, false, os.Stdout))
		return
	}

	coloredStatus := ansi.ColorizeStatus(payload.Status)

	url := urlForRequestID(&payload)
	requestLink := ansi.Linkify(payload.RequestID, url, os.Stdout)

	if payload.URL == "" {
		payload.URL = "[View path in dashboard]"
	}

	exampleLayout := "2006-01-02 15:04:05"
	localTime := time.Unix(int64(payload.CreatedAt), 0).Format(exampleLayout)

	color := ansi.Color(os.Stdout)
	outputStr := fmt.Sprintf("%s [%d] %s %s [%s]", color.Faint(localTime), coloredStatus, payload.Method, payload.URL, requestLink)
	fmt.Println(outputStr)

	errorValues := reflect.ValueOf(&payload.Error).Elem()
	errType := errorValues.Type()

	for i := 0; i < errorValues.NumField(); i++ {
		fieldValue := errorValues.Field(i).Interface()
		if fieldValue != "" {
			fmt.Printf("%s: %s\n", errType.Field(i).Name, fieldValue)
		}
	}
}

func jsonifyFilters(logFilters *LogFilters) (string, error) {
	bytes, err := json.Marshal(logFilters)
	if err != nil {
		return "", err
	}

	jsonStr := string(bytes)

	return jsonStr, nil
}

func urlForRequestID(payload *EventPayload) string {
	maybeTest := ""
	if !payload.Livemode {
		maybeTest = "/test"
	}

	return fmt.Sprintf("https://dashboard.stripe.com%s/logs/%s", maybeTest, payload.RequestID)
}
