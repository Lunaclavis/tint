/*
Package tint implements a zero-dependency [slog.Handler] that writes tinted
(colorized) logs. The output format is inspired by the [zerolog.ConsoleWriter]
and [slog.TextHandler].

The output format can be customized using [Options], which is a drop-in
replacement for [slog.HandlerOptions].

# Customize Attributes

Options.ReplaceAttr can be used to alter or drop attributes.
If set, it is called on each non-group attribute before it is logged.
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
	"encoding"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// ANSI modes
	ansiEsc          rune   = '\u001b'
	ansiReset        string = "\u001b[0m"
	ansiFaint        string = "\u001b[2m"
	ansiResetFaint   string = "\u001b[22m"
	ansiBrightRed    string = "\u001b[91m"
	ansiBrightYellow string = "\u001b[93m"
	ansiBrightBlue   string = "\u001b[94m"
	ansiBrightCyan   string = "\u001b[96m"

	errKey string = "err"

	defaultLevel slog.Level = slog.LevelInfo
	// defaultTimeFormat string = time.StampMilli

	noColor int16 = -1
)

// Options for a slog.Handler that writes tinted logs.
// A zero Options consists entirely of default values.
//
// Options can be used as a drop-in replacement for [slog.HandlerOptions].
type Options struct {
	// Enable source code location (Default: false)
	AddSource bool

	// Minimum level to log (Default: slog.LevelInfo)
	Level slog.Leveler

	// ReplaceAttr is called to rewrite each non-group attribute before it is logged.
	// See https://pkg.go.dev/log/slog#HandlerOptions for details.
	ReplaceAttr func(groups []string, attr slog.Attr) slog.Attr

	// Time format (Default: RFC3339 format with milliseconds)
	TimeFormat string

	// Disable color (Default: false)
	NoColor bool
}

// textHandler implements a [slog.Handler].
type textHandler struct {
	attrsPrefix string
	groupPrefix string
	groups      []string

	mu *sync.Mutex
	w  io.Writer

	opts Options
}

// NewTextHandler creates a [slog.Handler] that writes tinted logs to Writer w,
// using the default options. If opts is nil, the default options are used.
func NewTextHandler(w io.Writer, opts *Options) slog.Handler {
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.Level == nil {
		o.Level = defaultLevel
	}
	// if o.TimeFormat == "" {
	// 	o.TimeFormat = defaultTimeFormat
	// }

	return &textHandler{
		mu:   &sync.Mutex{},
		w:    w,
		opts: o,
	}
}

func (h *textHandler) clone() *textHandler {
	return &textHandler{
		attrsPrefix: h.attrsPrefix,
		groupPrefix: h.groupPrefix,
		groups:      slices.Clip(h.groups),
		mu:          h.mu, // mutex shared among all clones of this handler
		w:           h.w,
		opts:        h.opts,
	}
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	// get a buffer from the sync pool
	buf := newBuffer()
	defer buf.Release()

	rep := h.opts.ReplaceAttr

	// write time
	if !r.Time.IsZero() {
		if rep == nil {
			h.appendTintTime(buf, r.Time, noColor)
			buf.WriteByte(' ')
		} else {
			val := r.Time.Round(0) // strip monotonic to match Attr behavior
			if a := rep(nil, slog.Time(slog.TimeKey, val)); a.Key != "" {
				val, color := h.resolve(a.Value, noColor)
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
		h.appendTintLevel(buf, r.Level, noColor)
		buf.WriteByte(' ')
	} else if a := rep(nil, slog.Any(slog.LevelKey, r.Level)); a.Key != "" {
		val, color := h.resolve(a.Value, noColor)
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
				val, color := h.resolve(a.Value, noColor)
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
		val, color := h.resolve(a.Value, noColor)
		h.appendTintValue(buf, val, false, color, false)
		buf.WriteByte(' ')
	}

	// write handler attributes
	if len(h.attrsPrefix) > 0 {
		buf.WriteString(h.attrsPrefix)
	}

	// write attributes
	groups := slices.Clip(h.groups)
	r.Attrs(func(attr slog.Attr) bool {
		h.appendAttr(buf, attr, h.groupPrefix, groups, noColor)
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
	handler := h.clone()

	buf := newBuffer()
	defer buf.Release()

	// write attributes to buffer
	groups := slices.Clip(h.groups)
	for _, attr := range attrs {
		h.appendAttr(buf, attr, h.groupPrefix, groups, noColor)
	}
	handler.attrsPrefix = h.attrsPrefix + string(*buf)
	return handler
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	handler := h.clone()
	handler.groupPrefix += name + "."
	handler.groups = append(handler.groups, name)
	return handler
}

func (h *textHandler) appendTintTime(buf *buffer, t time.Time, color int16) {
	if h.opts.NoColor {
		h.appendTime(buf, t)
		return
	}

	switch {
	case color < 0:
		buf.WriteString(ansiFaint)
	default:
		appendAnsi(buf, uint8(color), true)
	}
	h.appendTime(buf, t)
	buf.WriteString(ansiReset)
}

func (h *textHandler) appendTime(buf *buffer, t time.Time) {
	if h.opts.TimeFormat == "" {
		*buf = appendRFC3339Millis(*buf, t)
	} else {
		*buf = t.AppendFormat(*buf, h.opts.TimeFormat)
	}
}

func (h *textHandler) appendTintLevel(buf *buffer, level slog.Level, color int16) {
	toBytes := func(base string, val slog.Level) []byte {
		if val == 0 {
			return []byte(base)
		}
		if val > 0 {
			return strconv.AppendInt(append([]byte(base), '+'), int64(val), 10)
		}
		return strconv.AppendInt([]byte(base), int64(val), 10)
	}

	if !h.opts.NoColor {
		if color >= 0 {
			appendAnsi(buf, uint8(color), false)
		} else {
			switch {
			case level < slog.LevelInfo:
				buf.WriteString(ansiBrightBlue)
			case level < slog.LevelWarn:
				buf.WriteString(ansiBrightCyan)
			case level < slog.LevelError:
				buf.WriteString(ansiBrightYellow)
			default:
				buf.WriteString(ansiBrightRed)
			}
		}
	}

	switch {
	case level < slog.LevelInfo:
		buf.Write(toBytes("DBG", level-slog.LevelDebug))
	case level < slog.LevelWarn:
		buf.Write(toBytes("INF", level-slog.LevelInfo))
	case level < slog.LevelError:
		buf.Write(toBytes("WRN", level-slog.LevelWarn))
	default:
		buf.Write(toBytes("ERR", level-slog.LevelError))
	}

	if !h.opts.NoColor {
		buf.WriteString(ansiReset)
	}
}

func appendSource(buf *buffer, src *slog.Source) {
	dir, file := filepath.Split(src.File)

	buf.WriteString(filepath.Join(filepath.Base(dir), file))
	buf.WriteByte(':')
	*buf = strconv.AppendInt(*buf, int64(src.Line), 10)
}

func (h *textHandler) resolve(val slog.Value, inheritedColor int16) (resolvedVal slog.Value, color int16) {
	if !h.opts.NoColor && val.Kind() == slog.KindLogValuer {
		if tintVal, ok := val.Any().(tintValue); ok {
			return tintVal.Value.Resolve(), int16(tintVal.Color)
		}
	}
	return val.Resolve(), inheritedColor
}

func (h *textHandler) appendAttr(buf *buffer, attr slog.Attr, groupsPrefix string, groups []string, color int16) {
	attr.Value, color = h.resolve(attr.Value, color)
	rep := h.opts.ReplaceAttr
	if rep != nil && attr.Value.Kind() != slog.KindGroup {
		attr = rep(groups, attr)
		attr.Value, color = h.resolve(attr.Value, color)
	}

	if attr.Equal(slog.Attr{}) {
		return
	}

	if attr.Value.Kind() == slog.KindGroup {
		if attr.Key != "" {
			groupsPrefix += attr.Key + "."
			if rep != nil {
				groups = append(groups, attr.Key)
			}
		}
		for _, groupAttr := range attr.Value.Group() {
			h.appendAttr(buf, groupAttr, groupsPrefix, groups, color)
		}
		return
	}

	if h.opts.NoColor {
		h.appendKey(buf, attr.Key, groupsPrefix)
		h.appendValue(buf, attr.Value, true)
	} else {
		if color >= 0 {
			appendAnsi(buf, uint8(color), true)
			h.appendKey(buf, attr.Key, groupsPrefix)
			buf.WriteString(ansiResetFaint)
			h.appendValue(buf, attr.Value, true)
			buf.WriteString(ansiReset)
		} else {
			buf.WriteString(ansiFaint)
			h.appendKey(buf, attr.Key, groupsPrefix)
			buf.WriteString(ansiReset)
			h.appendValue(buf, attr.Value, true)
		}
	}
	buf.WriteByte(' ')
}

func (h *textHandler) appendKey(buf *buffer, key, groups string) {
	appendString(buf, groups+key, true, !h.opts.NoColor)
	buf.WriteByte('=')
}

func (h *textHandler) appendValue(buf *buffer, v slog.Value, quote bool) {
	switch v.Kind() {
	case slog.KindString:
		appendString(buf, v.String(), quote, !h.opts.NoColor)
	case slog.KindInt64:
		*buf = strconv.AppendInt(*buf, v.Int64(), 10)
	case slog.KindUint64:
		*buf = strconv.AppendUint(*buf, v.Uint64(), 10)
	case slog.KindFloat64:
		*buf = strconv.AppendFloat(*buf, v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		*buf = strconv.AppendBool(*buf, v.Bool())
	case slog.KindDuration:
		appendString(buf, v.Duration().String(), quote, !h.opts.NoColor)
	case slog.KindTime:
		*buf = appendRFC3339Millis(*buf, v.Time())
	case slog.KindAny:
		defer func() {
			// Copied from log/slog/handler.go.
			if r := recover(); r != nil {
				// If it panics with a nil pointer, the most likely cases are
				// an encoding.TextMarshaler or error fails to guard against nil,
				// in which case "<nil>" seems to be the feasible choice.
				//
				// Adapted from the code in fmt/print.go.
				if v := reflect.ValueOf(v.Any()); v.Kind() == reflect.Pointer && v.IsNil() {
					buf.WriteString("<nil>")
					return
				}

				// Otherwise just print the original panic message.
				appendString(buf, fmt.Sprintf("!PANIC: %v", r), true, !h.opts.NoColor)
			}
		}()

		switch cv := v.Any().(type) {
		case encoding.TextMarshaler:
			data, err := cv.MarshalText()
			if err != nil {
				break
			}
			appendString(buf, string(data), quote, !h.opts.NoColor)
		case *slog.Source:
			appendSource(buf, cv)
		default:
			if bs, ok := byteSlice(cv); ok {
				if quote {
					*buf = strconv.AppendQuote(*buf, string(bs))
				} else {
					buf.Write(bs)
				}
				break
			}
			appendString(buf, fmt.Sprintf("%+v", cv), quote, !h.opts.NoColor)
		}
	}
}

// byteSlice returns its argument as a []byte if the argument's
// underlying type is []byte, along with a second return value of true.
// Otherwise it returns nil, false.
//
// Copied from log/slog/text_handler.go.
func byteSlice(a any) ([]byte, bool) {
	if bs, ok := a.([]byte); ok {
		return bs, true
	}
	// Like Printf's %s, we allow both the slice type and the byte element type to be named.
	t := reflect.TypeOf(a)
	if t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return reflect.ValueOf(a).Bytes(), true
	}
	return nil, false
}

func (h *textHandler) appendTintValue(buf *buffer, val slog.Value, quote bool, color int16, faint bool) {
	if h.opts.NoColor {
		h.appendValue(buf, val, quote)
	} else {
		if color >= 0 {
			appendAnsi(buf, uint8(color), faint)
		} else if faint {
			buf.WriteString(ansiFaint)
		}
		h.appendValue(buf, val, quote)
		if color >= 0 || faint {
			buf.WriteString(ansiReset)
		}
	}
}

// Copied from log/slog/handler.go.
func appendRFC3339Millis(b []byte, t time.Time) []byte {
	// Format according to time.RFC3339Nano since it is highly optimized,
	// but truncate it to use millisecond resolution.
	// Unfortunately, that format trims trailing 0s, so add 1/10 millisecond
	// to guarantee that there are exactly 4 digits after the period.
	const prefixLen = len("2006-01-02T15:04:05.000")
	n := len(b)
	t = t.Truncate(time.Millisecond).Add(time.Millisecond / 10)
	b = t.AppendFormat(b, time.RFC3339Nano)
	b = append(b[:n+prefixLen], b[n+prefixLen+1:]...) // drop the 4th digit
	return b
}

func appendAnsi(buf *buffer, color uint8, faint bool) {
	buf.WriteString("\u001b[")
	if faint {
		buf.WriteString("2;")
	}
	switch {
	case color < 8:
		*buf = strconv.AppendUint(*buf, uint64(color)+30, 10)
	case color < 16:
		*buf = strconv.AppendUint(*buf, uint64(color)+82, 10)
	default:
		buf.WriteString("38;5;")
		*buf = strconv.AppendUint(*buf, uint64(color), 10)
	}
	buf.WriteByte('m')
}

func appendString(buf *buffer, s string, quote, color bool) {
	if quote && !color {
		// trim ANSI escape sequences
		s = trimEscape(s)
	}

	quote = quote && needsQuoting(s)
	switch {
	case color && quote:
		s = strconv.Quote(s)
		s = strings.ReplaceAll(s, `\x1b`, string(ansiEsc))
		buf.WriteString(s)
	case !color && quote:
		*buf = strconv.AppendQuote(*buf, s)
	default:
		buf.WriteString(s)
	}
}

func trimEscape(s string) string {
	i := 0
	l := len(s)
	for i < l {
		if s[i] == byte(ansiEsc) {
			break
		}
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError {
			return s[:i]
		}
		i += size
	}
	if i == l {
		return s
	}

	res := make([]byte, 0, l)
	res = append(res, s[:i]...)

	var inEscape bool
	for i < l {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError {
			break
		}
		switch {
		case r == ansiEsc:
			inEscape = true
		case inEscape:
			if unicode.IsLetter(r) {
				inEscape = false
			}
		default:
			res = append(res, s[i:i+size]...)
		}
		i += size
	}

	return string(res)
}

// Copied from log/slog/text_handler.go.
func needsQuoting(s string) bool {
	if len(s) == 0 {
		return true
	}
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			// Quote anything except a backslash that would need quoting in a
			// JSON string, as well as space and '='
			if b != '\\' && (b == ' ' || b == '=' || !safeSet[b]) {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
		i += size
	}
	return false
}

// Copied from log/slog/json_handler.go.
//
// safeSet is extended by the ANSI escape code "\u001b".
var safeSet = [utf8.RuneSelf]bool{
	' ':      true,
	'!':      true,
	'"':      false,
	'#':      true,
	'$':      true,
	'%':      true,
	'&':      true,
	'\'':     true,
	'(':      true,
	')':      true,
	'*':      true,
	'+':      true,
	',':      true,
	'-':      true,
	'.':      true,
	'/':      true,
	'0':      true,
	'1':      true,
	'2':      true,
	'3':      true,
	'4':      true,
	'5':      true,
	'6':      true,
	'7':      true,
	'8':      true,
	'9':      true,
	':':      true,
	';':      true,
	'<':      true,
	'=':      true,
	'>':      true,
	'?':      true,
	'@':      true,
	'A':      true,
	'B':      true,
	'C':      true,
	'D':      true,
	'E':      true,
	'F':      true,
	'G':      true,
	'H':      true,
	'I':      true,
	'J':      true,
	'K':      true,
	'L':      true,
	'M':      true,
	'N':      true,
	'O':      true,
	'P':      true,
	'Q':      true,
	'R':      true,
	'S':      true,
	'T':      true,
	'U':      true,
	'V':      true,
	'W':      true,
	'X':      true,
	'Y':      true,
	'Z':      true,
	'[':      true,
	'\\':     false,
	']':      true,
	'^':      true,
	'_':      true,
	'`':      true,
	'a':      true,
	'b':      true,
	'c':      true,
	'd':      true,
	'e':      true,
	'f':      true,
	'g':      true,
	'h':      true,
	'i':      true,
	'j':      true,
	'k':      true,
	'l':      true,
	'm':      true,
	'n':      true,
	'o':      true,
	'p':      true,
	'q':      true,
	'r':      true,
	's':      true,
	't':      true,
	'u':      true,
	'v':      true,
	'w':      true,
	'x':      true,
	'y':      true,
	'z':      true,
	'{':      true,
	'|':      true,
	'}':      true,
	'~':      true,
	'\u007f': true,
	'\u001b': true,
}

type tintValue struct {
	slog.Value
	Color uint8
}

// LogValue implements the [slog.LogValuer] interface.
func (v tintValue) LogValue() slog.Value {
	return v.Value
}

// Err returns a tinted (colorized) [slog.Attr] that will be written in red color.
// When used with any other [slog.Handler], it behaves as
//
//	slog.Any("err", err)
func Err(err error) slog.Attr {
	return Attr(9, slog.Any(errKey, err))
}

// Attr returns a tinted (colorized) [slog.Attr] that will be written in the
// specified color.
// When used with any other [slog.Handler], it behaves as a plain [slog.Attr].
//
// Use the uint8 color value to specify the color of the attribute:
//
//   - 0-7: standard ANSI colors
//   - 8-15: high intensity ANSI colors
//   - 16-231: 216 colors (6×6×6 cube)
//   - 232-255: grayscale from dark to light in 24 steps
//
// See https://en.wikipedia.org/wiki/ANSI_escape_code#8-bit
func Attr(color uint8, attr slog.Attr) slog.Attr {
	attr.Value = slog.AnyValue(tintValue{attr.Value, color})
	return attr
}
