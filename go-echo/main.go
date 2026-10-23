// go-echo is a media endpoint for Asterisk's chan_websocket: every binary
// (audio) frame received is written straight back, so the caller hears an echo.
//
// LOG_LEVEL controls verbosity:
//
//	info  (default) connect, MEDIA_START, DTMF, XOFF/XON, per-call totals
//	debug           + periodic media stats, Asterisk STATUS polling, channel vars
//	trace           + request headers, raw control JSON, every media frame
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const levelTrace = slog.Level(-8)

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"media"},
	CheckOrigin:  func(*http.Request) bool { return true },
}

// statsInterval controls how often media stats are logged and GET_STATUS is sent (debug level).
var statsInterval = 5 * time.Second

// event is the subset of chan_websocket JSON control events we care about.
type event struct {
	Event            string            `json:"event"`
	ConnectionID     string            `json:"connection_id"`
	Channel          string            `json:"channel"`
	ChannelID        string            `json:"channel_id"`
	Format           string            `json:"format"`
	OptimalFrameSize int               `json:"optimal_frame_size"`
	Ptime            int               `json:"ptime"`
	Digit            string            `json:"digit"`
	ChannelVariables map[string]string `json:"channel_variables"`
	// TRANSFER_RESULT / TRANSFER_PROGRESS (n2p-asterisk websocket-transfer patch)
	CorrelationID string `json:"correlation_id"`
	Status        string `json:"status"`
	Cause         string `json:"cause"`
	State         string `json:"state"`
}

// transferCommand is the chan_websocket TRANSFER command (cold transfer).
type transferCommand struct {
	Command       string `json:"command"`
	CorrelationID string `json:"correlation_id"`
	Exten         string `json:"exten"`
	Context       string `json:"context"`
}

// Transfer target for DTMF 0 / TRANSFER_AFTER; the context must be in chan_websocket.conf transfer_contexts.
var (
	transferExten   = "601"
	transferContext = "internal"
	transferAfter   time.Duration // 0 = off
)

// mediaStats accumulates per-interval media statistics (debug level only).
type mediaStats struct {
	frames        int
	bytes         int
	lastFrame     time.Time
	gapSum        time.Duration
	gapMax        time.Duration
	gapCount      int
	late          int // gaps > 1.5 * ptime
	sumSquares    float64
	samples       int
	peak          int
	silentFrames  int
	sizes         map[int]int
	intervalStart time.Time
}

func (s *mediaStats) reset(now time.Time) {
	last := s.lastFrame
	*s = mediaStats{sizes: map[int]int{}, intervalStart: now, lastFrame: last}
}

// add records one inbound ulaw frame.
func (s *mediaStats) add(now time.Time, data []byte, ptime time.Duration) {
	s.frames++
	s.bytes += len(data)
	s.sizes[len(data)]++

	if !s.lastFrame.IsZero() {
		gap := now.Sub(s.lastFrame)
		s.gapSum += gap
		s.gapCount++
		s.gapMax = max(s.gapMax, gap)
		if gap > ptime*3/2 {
			s.late++
		}
	}
	s.lastFrame = now

	var frameSq float64
	for _, b := range data {
		v := int(ulawToLinear(b))
		frameSq += float64(v * v)
		s.peak = max(s.peak, v, -v)
	}
	s.sumSquares += frameSq
	s.samples += len(data)
	if len(data) > 0 && dbfs(math.Sqrt(frameSq/float64(len(data)))) < -50 {
		s.silentFrames++
	}
}

func (s *mediaStats) log(log *slog.Logger, now time.Time, framesIn, framesOut, dropped int) {
	elapsed := now.Sub(s.intervalStart).Seconds()
	attrs := []any{
		"frames", s.frames,
		"fps", round1(float64(s.frames) / elapsed),
		"kbps", round1(float64(s.bytes*8) / elapsed / 1000),
		"sizes", s.sizes,
		"late_frames", s.late,
		"gap_max_ms", s.gapMax.Milliseconds(),
		"total_in", framesIn,
		"total_out", framesOut,
		"dropped", dropped,
	}
	if s.gapCount > 0 {
		attrs = append(attrs, "gap_avg_ms", round1(float64(s.gapSum.Microseconds())/float64(s.gapCount)/1000))
	}
	if s.samples > 0 {
		attrs = append(attrs,
			"rms_dbfs", round1(dbfs(math.Sqrt(s.sumSquares/float64(s.samples)))),
			"peak_dbfs", round1(dbfs(float64(s.peak))),
			"silent_pct", round1(100*float64(s.silentFrames)/float64(s.frames)),
		)
	}
	log.Debug("media stats", attrs...)
}

func handleMedia(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("upgrade failed", "err", err)
		return
	}
	defer conn.Close()

	ctx := r.Context()
	log := slog.With("remote", r.RemoteAddr)
	debug := log.Enabled(ctx, slog.LevelDebug)
	trace := log.Enabled(ctx, levelTrace)

	log.Info("connected", "subprotocol", conn.Subprotocol())
	log.Debug("request", "path", r.URL.Path, "query", r.URL.RawQuery, "user_agent", r.UserAgent())
	if trace {
		for name, values := range r.Header {
			log.Log(ctx, levelTrace, "request header", "name", name, "value", strings.Join(values, ", "))
		}
	}

	start := time.Now()
	var framesIn, framesOut, bytesIn, dropped int
	var firstFrame time.Time
	ptime := 20 * time.Millisecond
	paused := false // set by MEDIA_XOFF, cleared by MEDIA_XON
	stats := &mediaStats{}
	stats.reset(start)

	sendCommand := func(cmd string) {
		msg := `{"command":"` + cmd + `"}`
		log.Log(ctx, levelTrace, "send command", "msg", msg)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			log.Warn("send command", "command", cmd, "err", err)
		}
	}

	var connectionID string
	transfers := 0
	sendTransfer := func() {
		transfers++
		cmd := transferCommand{
			Command:       "TRANSFER",
			CorrelationID: "xfer-" + connectionID + "-" + strconv.Itoa(transfers),
			Exten:         transferExten,
			Context:       transferContext,
		}
		msg, _ := json.Marshal(cmd)
		log.Info("transferring", "exten", cmd.Exten, "context", cmd.Context, "correlation_id", cmd.CorrelationID)
		log.Log(ctx, levelTrace, "send command", "msg", string(msg))
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Warn("send command", "command", cmd.Command, "err", err)
		}
	}
	// TRANSFER_AFTER fires on the read loop (the only writer) via the first frame after the deadline.
	var transferAt time.Time

	defer func() {
		now := time.Now()
		if debug && stats.frames > 0 {
			stats.log(log, now, framesIn, framesOut, dropped)
		}
		log.Info("disconnected", "duration", now.Sub(start).Round(time.Millisecond),
			"frames_in", framesIn, "frames_out", framesOut, "bytes_in", bytesIn)
		if debug {
			attrs := []any{"dropped", dropped}
			if !firstFrame.IsZero() {
				attrs = append(attrs, "first_frame_after", firstFrame.Sub(start).Round(time.Millisecond))
			}
			log.Debug("call summary", attrs...)
		}
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				log.Debug("close frame", "code", ce.Code, "text", ce.Text)
			}
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Warn("read", "err", err)
			}
			return
		}
		now := time.Now()

		switch mt {
		case websocket.TextMessage:
			log.Log(ctx, levelTrace, "control raw", "msg", string(data))
			var ev event
			if err := json.Unmarshal(data, &ev); err != nil {
				log.Info("control (non-JSON)", "msg", string(data))
				continue
			}
			switch ev.Event {
			case "MEDIA_START":
				log = log.With("channel", ev.Channel)
				connectionID = ev.ConnectionID
				if transferAfter > 0 {
					transferAt = now.Add(transferAfter)
				}
				if ev.Ptime > 0 {
					ptime = time.Duration(ev.Ptime) * time.Millisecond
				}
				log.Info("MEDIA_START", "connection_id", ev.ConnectionID, "format", ev.Format,
					"optimal_frame_size", ev.OptimalFrameSize, "ptime", ev.Ptime)
				log.Debug("MEDIA_START details", "channel_id", ev.ChannelID, "channel_variables", ev.ChannelVariables)
				if debug {
					if ev.Format != "ulaw" {
						log.Warn("audio level stats assume ulaw", "format", ev.Format)
					}
					sendCommand("GET_STATUS")
				}
			case "DTMF_END":
				log.Info("DTMF", "digit", ev.Digit)
				switch ev.Digit {
				case "#":
					log.Info("hanging up on #")
					sendCommand("HANGUP")
				case "0":
					sendTransfer()
				case "*":
					if debug {
						sendCommand("GET_STATUS")
					}
				}
			case "MEDIA_XOFF":
				paused = true
				log.Warn("MEDIA_XOFF")
			case "MEDIA_XON":
				paused = false
				log.Info("MEDIA_XON")
			case "TRANSFER_RESULT":
				// On success Asterisk closes the socket right after this event.
				attrs := []any{"status", ev.Status, "cause", ev.Cause, "correlation_id", ev.CorrelationID}
				if ev.Status == "success" {
					log.Info("TRANSFER_RESULT", attrs...)
				} else {
					log.Warn("TRANSFER_RESULT", attrs...)
				}
			case "TRANSFER_PROGRESS":
				log.Info("TRANSFER_PROGRESS", "state", ev.State, "correlation_id", ev.CorrelationID)
			default:
				// STATUS, QUEUE_DRAINED, MEDIA_MARK_PROCESSED, etc.
				log.Debug("event", "name", ev.Event, "msg", string(data))
			}

		case websocket.BinaryMessage:
			if firstFrame.IsZero() {
				firstFrame = now
			}
			framesIn++
			bytesIn += len(data)
			if debug {
				stats.add(now, data, ptime)
			}
			if trace {
				log.Log(ctx, levelTrace, "frame", "n", framesIn, "bytes", len(data))
			}

			if paused {
				dropped++
			} else if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				// Frames arrive paced at ptime and sized at optimal_frame_size,
				// so echoing them 1:1 keeps both size and timing valid.
				log.Warn("write", "err", err)
				return
			} else {
				framesOut++
			}

			if !transferAt.IsZero() && !now.Before(transferAt) {
				transferAt = time.Time{}
				sendTransfer()
			}

			if debug && now.Sub(stats.intervalStart) >= statsInterval {
				stats.log(log, now, framesIn, framesOut, dropped)
				stats.reset(now)
				sendCommand("GET_STATUS")
			}
		}
	}
}

// ulawToLinear decodes one G.711 mu-law byte to a 16-bit linear sample.
func ulawToLinear(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0F
	sample := ((int(mantissa) << 3) + 0x84) << exponent
	sample -= 0x84
	if sign != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

func dbfs(v float64) float64 {
	if v <= 0 {
		return -96
	}
	return 20 * math.Log10(v/32768)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return levelTrace
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func main() {
	level := parseLevel(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	if v := os.Getenv("STATS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			statsInterval = d
		}
	}

	if v := os.Getenv("TRANSFER_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			transferAfter = d
		}
	}
	if v := os.Getenv("TRANSFER_EXTEN"); v != "" {
		transferExten = v
	}
	if v := os.Getenv("TRANSFER_CONTEXT"); v != "" {
		transferContext = v
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/media", handleMedia)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	srv := &http.Server{Addr: addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", addr, "log_level", os.Getenv("LOG_LEVEL"))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
