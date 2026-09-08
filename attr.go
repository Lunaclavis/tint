package tint

import (
	"encoding"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ansiEsc rune = '\u001b'

	ansiReset         string = "\u001b[0m"
	ansiFaint         string = "\u001b[2m"
	ansiResetFaint    string = "\u001b[22m"
	ansiBrightRed     string = "\u001b[91m"
	ansiBrightGreen   string = "\u001b[92m"
	ansiBrightYellow  string = "\u001b[93m"
	ansiBrightBlue    string = "\u001b[94m"
	ansiBrightMagenta string = "\u001b[95m"
	ansiBrightCyan    string = "\u001b[96m"

	errKey string = "err"

	levelUnknown slog.Level = slog.Level(12)
)

func (h *textHandler) appendTintTime(buf *buffer, t time.Time, color int16) {
	if h.opts.NoColor {
		if h.opts.TimeFormat == defaultTimeFormat {
			*buf = appendRFC3339Millis(*buf, t)
		} else {
			*buf = t.AppendFormat(*buf, h.opts.TimeFormat)
		}
		return
	}

	if color < 0 {
		buf.WriteString(ansiBrightGreen)
	} else {
		appendAnsi(buf, uint8(color), false)
	}

	if h.opts.TimeFormat == defaultTimeFormat {
		*buf = appendRFC3339Millis(*buf, t)
	} else {
		*buf = t.AppendFormat(*buf, h.opts.TimeFormat)
	}

	buf.WriteString(ansiReset)
}

func (h *textHandler) appendTintLevel(buf *buffer, level slog.Level, color int16) {
	str := func(base string, val slog.Level) []byte {
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
			case level < levelUnknown:
				buf.WriteString(ansiBrightMagenta)
			default:
				buf.WriteString(ansiBrightRed)
			}
		}
	}

	switch {
	case level < slog.LevelInfo:
		buf.Write(str("DEBUG  ", level-slog.LevelDebug))
	case level < slog.LevelWarn:
		buf.Write(str("INFO   ", level-slog.LevelInfo))
	case level < slog.LevelError:
		buf.Write(str("WARNING", level-slog.LevelWarn))
	case level < levelUnknown:
		buf.Write(str("ERROR  ", level-slog.LevelError))
	default:
		buf.Write([]byte("FATAL  "))
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

func (h *textHandler) resolve(val slog.Value) (resolvedVal slog.Value, color int16) {
	if !h.opts.NoColor && val.Kind() == slog.KindLogValuer {
		if tintVal, ok := val.Any().(tintValue); ok {
			return tintVal.Value.Resolve(), int16(tintVal.Color)
		}
	}
	return val.Resolve(), -1
}

func (h *textHandler) appendAttr(buf *buffer, attr slog.Attr, groupsPrefix string, groups []string) {
	var color int16 // -1 if no color
	attr.Value, color = h.resolve(attr.Value)
	if rep := h.opts.ReplaceAttr; rep != nil && attr.Value.Kind() != slog.KindGroup {
		attr = rep(groups, attr)
		var colorRep int16
		attr.Value, colorRep = h.resolve(attr.Value)
		if colorRep >= 0 {
			color = colorRep
		}
	}

	if attr.Equal(slog.Attr{}) {
		return
	}

	if attr.Value.Kind() == slog.KindGroup {
		if attr.Key != "" {
			groupsPrefix += attr.Key + "."
			groups = append(groups, attr.Key)
		}
		for _, groupAttr := range attr.Value.Group() {
			h.appendAttr(buf, groupAttr, groupsPrefix, groups)
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
		var inEscape bool
		s = cut(s, func(r rune) bool {
			if r == ansiEsc {
				inEscape = true
			} else if inEscape && unicode.IsLetter(r) {
				inEscape = false
				return true
			}
			return inEscape
		})
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

func cut(s string, f func(r rune) bool) string {
	var res []rune
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError {
			break
		}
		if !f(r) {
			res = append(res, r)
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
	Value slog.Value
	Color uint8
}

// LogValue implements the [slog.LogValuer] interface.
func (v tintValue) LogValue() slog.Value {
	return v.Value
}

// Err returns a tinted (colorized) [slog.Attr] that will be written in red color
// by the [tint.Handler]. When used with any other [slog.Handler], it behaves as
//
//	slog.Any("err", err)
func Err(err error) slog.Attr {
	return Attr(9, slog.Any(errKey, err))
}

// Attr returns a tinted (colorized) [slog.Attr] that will be written in the
// specified color by the [tint.Handler]. When used with any other [slog.Handler], it behaves as a
// plain [slog.Attr].
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

var _ slog.LogValuer = tintValue{}
