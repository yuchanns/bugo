package logging

import (
	"encoding/json"
	"fmt"
	stdlog "log"
	"os"
	"regexp"
	"strings"

	phuslog "github.com/phuslu/log"
)

var mediaDataURLRe = regexp.MustCompile(`data:[^;\s]+;base64,[^"'\s]+`)

type Event struct {
	entry *phuslog.Entry
}

func Configure() {
	phuslog.DefaultLogger = phuslog.Logger{
		Level:      phuslog.InfoLevel,
		TimeFormat: "",
		Writer: &phuslog.ConsoleWriter{
			Writer:         os.Stderr,
			ColorOutput:    false,
			QuoteString:    true,
			EndWithMessage: false,
		},
	}

	stdlog.SetFlags(0)
	stdlog.SetOutput(stdlibBridge{})
}

func Trace() *Event { return wrap(phuslog.Trace()) }
func Debug() *Event { return wrap(phuslog.Debug()) }
func Info() *Event  { return wrap(phuslog.Info()) }
func Warn() *Event  { return wrap(phuslog.Warn()) }
func Error() *Event { return wrap(phuslog.Error()) }
func Fatal() *Event { return wrap(phuslog.Fatal()) }
func Panic() *Event { return wrap(phuslog.Panic()) }

func (e *Event) Str(key, value string) *Event {
	if e != nil && e.entry != nil {
		e.entry.Str(key, value)
	}
	return e
}

func (e *Event) Int(key string, value int) *Event {
	if e != nil && e.entry != nil {
		e.entry.Int(key, value)
	}
	return e
}

func (e *Event) Int64(key string, value int64) *Event {
	if e != nil && e.entry != nil {
		e.entry.Int64(key, value)
	}
	return e
}

func (e *Event) Bool(key string, value bool) *Event {
	if e != nil && e.entry != nil {
		e.entry.Bool(key, value)
	}
	return e
}

func (e *Event) Float64(key string, value float64) *Event {
	if e != nil && e.entry != nil {
		e.entry.Float64(key, value)
	}
	return e
}

func (e *Event) Err(err error) *Event {
	if e != nil && e.entry != nil && err != nil {
		e.entry.Err(err)
	}
	return e
}

func (e *Event) Msg(msg string) {
	if e != nil && e.entry != nil {
		e.entry.Msg(msg)
	}
}

func (e *Event) Msgf(format string, args ...any) {
	if e != nil && e.entry != nil {
		e.entry.Msgf(format, args...)
	}
}

func PrettifyText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	text = mediaDataURLRe.ReplaceAllString(text, "[media]")
	return shorten(text, 240)
}

func RenderValue(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return PrettifyText(fmt.Sprintf("%v", value))
	}
	return PrettifyText(string(b))
}

func wrap(entry *phuslog.Entry) *Event {
	return &Event{entry: entry}
}

type stdlibBridge struct{}

func (stdlibBridge) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}

	component := "stdlib"
	event := Info()

	if after, ok := strings.CutPrefix(msg, "[TGBOT] "); ok {
		component = "tgbot"
		msg = after
		switch {
		case strings.HasPrefix(msg, "[ERROR] "):
			event = Error()
			msg = strings.TrimPrefix(msg, "[ERROR] ")
		case strings.HasPrefix(msg, "[DEBUG] "):
			event = Debug()
			msg = strings.TrimPrefix(msg, "[DEBUG] ")
		case strings.HasPrefix(msg, "[UPDATE] "):
			event = Debug()
			msg = strings.TrimPrefix(msg, "[UPDATE] ")
		}
	}

	event.Str("component", component).Msg(strings.TrimSpace(msg))
	return len(p), nil
}

func shorten(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	if limit <= 3 {
		return text[:limit]
	}
	return text[:limit-3] + "..."
}
