/*
Package tint implements a zero-dependency [slog.Handler] that writes tinted
(colorized) logs. The output format is inspired by the [zerolog.ConsoleWriter]
and [slog.TextHandler].

The output format can be customized using [Options], which is a drop-in
replacement for [slog.HandlerOptions].

# Customize Attributes

Options.ReplaceAttr can be used to alter or drop attributes. If set, it is
called on each non-group attribute before it is logged.
See [slog.HandlerOptions] for details.

Create a new logger with a custom TRACE level:

	const LevelTrace = slog.LevelDebug - 4

	w := os.Stderr
	logger := slog.New(tint.NewTextHandler(w, &tint.Options{
		Level: LevelTrace,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey && len(groups) == 0 {
				level, ok := a.Value.Any().(slog.Level)
				if ok && level <= LevelTrace {
					return tint.Attr(13, slog.String(a.Key, "TRC"))
				}
			}
			return a
		},
	}))

Create a new logger that doesn't write the time:

	w := os.Stderr
	logger := slog.New(
		tint.NewTextHandler(w, &tint.Options{
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey && len(groups) == 0 {
					return slog.Attr{}
				}
				return a
			},
		}),
	)

Create a new logger that writes all errors in red:

	w := os.Stderr
	logger := slog.New(
		tint.NewTextHandler(w, &tint.Options{
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Value.Kind() == slog.KindAny {
					if _, ok := a.Value.Any().(error); ok {
						return tint.Attr(9, a)
					}
				}
				return a
			},
		}),
	)

# Automatically Enable Colors

Colors are enabled by default. Use the Options.NoColor field to disable
color output. To automatically enable colors based on terminal capabilities, use
e.g., the [go-isatty] package:

	w := os.Stderr
	logger := slog.New(
		tint.NewTextHandler(w, &tint.Options{
			NoColor: !isatty.IsTerminal(w.Fd()),
		}),
	)

# Windows Support

Color support on Windows can be added by using e.g., the [go-colorable] package:

	w := os.Stderr
	logger := slog.New(
		tint.NewTextHandler(colorable.NewColorable(w), nil),
	)

[zerolog.ConsoleWriter]: https://pkg.go.dev/github.com/rs/zerolog#ConsoleWriter
[go-isatty]: https://pkg.go.dev/github.com/mattn/go-isatty
[go-colorable]: https://pkg.go.dev/github.com/mattn/go-colorable
*/
package tint

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
)

const (
	defaultLevel      slog.Level = slog.LevelInfo
	defaultTimeFormat string     = "RFC3339Millis"
)

// Options for a slog.Handler that writes tinted logs. A zero Options consists
// entirely of default values.
//
// Options can be used as a drop-in replacement for [slog.HandlerOptions].
type HandlerOptions struct {
	// Enable source code location (Default: false)
	AddSource bool

	// Minimum level to log (Default: slog.LevelInfo)
	Level slog.Leveler

	// ReplaceAttr is called to rewrite each non-group attribute before it is logged.
	// See https://pkg.go.dev/log/slog#HandlerOptions for details.
	ReplaceAttr func(groups []string, attr slog.Attr) slog.Attr

	// Time format (Default: "RFC3339Millis")
	TimeFormat string

	// Disable color (Default: false)
	NoColor bool
}

func (o *HandlerOptions) setDefaults() {
	if o.Level == nil {
		o.Level = defaultLevel
	}
	if o.TimeFormat == "" {
		o.TimeFormat = defaultTimeFormat
	}
}

// handler implements a [slog.Handler].
type textHandler struct {
	opts        HandlerOptions
	attrsPrefix string
	groupPrefix string
	groups      []string
	mu          *sync.Mutex
	w           io.Writer
}

// NewTextHandler creates a [slog.Handler] that writes tinted logs to Writer w,
// using the default options. If opts is nil, the default options are used.
func NewTextHandler(w io.Writer, opts *HandlerOptions) slog.Handler {
	if opts == nil {
		opts = &HandlerOptions{}
	}
	opts.setDefaults()

	return &textHandler{
		opts: *opts,
		mu:   &sync.Mutex{},
		w:    w,
	}
}

func (h *textHandler) clone() *textHandler {
	return &textHandler{
		opts:        h.opts,
		attrsPrefix: h.attrsPrefix,
		groupPrefix: h.groupPrefix,
		groups:      h.groups,
		mu:          h.mu, // mutex shared among all clones of this handler
		w:           h.w,
	}
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	// get a buffer from the sync pool
	buf := newBuffer()
	defer buf.Free()

	rep := h.opts.ReplaceAttr

	// write time
	if !r.Time.IsZero() {
		if rep == nil {
			h.appendTintTime(buf, r.Time, -1)
			buf.WriteByte(' ')
		} else {
			val := r.Time.Round(0) // strip monotonic to match Attr behavior
			if a := rep(nil, slog.Time(slog.TimeKey, val)); a.Key != "" {
				val, color := h.resolve(a.Value)
				if val.Kind() == slog.KindTime {
					h.appendTintTime(buf, val.Time(), color)
				} else {
					h.appendTintValue(buf, val, false, color, true)
				}
				buf.WriteByte(' ')
			}
		}
	}

	// write level
	if rep == nil {
		h.appendTintLevel(buf, r.Level, -1)
		buf.WriteByte(' ')
	} else if a := rep(nil, slog.Any(slog.LevelKey, r.Level)); a.Key != "" {
		val, color := h.resolve(a.Value)
		if val.Kind() == slog.KindAny {
			if lvlVal, ok := val.Any().(slog.Level); ok {
				h.appendTintLevel(buf, lvlVal, color)
			} else {
				h.appendTintValue(buf, val, false, color, false)
			}
		} else {
			h.appendTintValue(buf, val, false, color, false)
		}
		buf.WriteByte(' ')
	}

	// write source
	if h.opts.AddSource {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		if f.File != "" {
			src := &slog.Source{
				Function: f.Function,
				File:     f.File,
				Line:     f.Line,
			}

			if rep == nil {
				if h.opts.NoColor {
					appendSource(buf, src)
				} else {
					buf.WriteString(ansiFaint)
					appendSource(buf, src)
					buf.WriteString(ansiReset)
				}
				buf.WriteByte(' ')
			} else if a := rep(nil, slog.Any(slog.SourceKey, src)); a.Key != "" {
				val, color := h.resolve(a.Value)
				h.appendTintValue(buf, val, false, color, true)
				buf.WriteByte(' ')
			}
		}
	}

	// write message
	if rep == nil {
		buf.WriteString(r.Message)
		buf.WriteByte(' ')
	} else if a := rep(nil, slog.String(slog.MessageKey, r.Message)); a.Key != "" {
		val, color := h.resolve(a.Value)
		h.appendTintValue(buf, val, false, color, false)
		buf.WriteByte(' ')
	}

	// write handler attributes
	if len(h.attrsPrefix) > 0 {
		buf.WriteString(h.attrsPrefix)
	}

	// write attributes
	r.Attrs(func(attr slog.Attr) bool {
		h.appendAttr(buf, attr, h.groupPrefix, h.groups)
		return true
	})

	if len(*buf) == 0 {
		buf.WriteByte('\n')
	} else {
		(*buf)[len(*buf)-1] = '\n' // replace last space with newline
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.w.Write(*buf)
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := h.clone()

	buf := newBuffer()
	defer buf.Free()

	// write attributes to buffer
	for _, attr := range attrs {
		h.appendAttr(buf, attr, h.groupPrefix, h.groups)
	}
	h2.attrsPrefix = h.attrsPrefix + string(*buf)
	return h2
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := h.clone()
	h2.groupPrefix += name + "."
	h2.groups = append(h2.groups, name)
	return h2
}
