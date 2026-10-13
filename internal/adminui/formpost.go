package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A page that takes writes decodes its own POST bodies: the plane mounts a
// page for every method and checks only the credential and that the request
// is same-site. The checks the plane made for a panel action are made here:
// a body cap, a JSON content type, exactly the declared fields once each,
// each of the declared kind and range, and a device that is registered now.
const (
	// formBodyMax caps the encoded body, so a text holding quotes or control
	// bytes, which JSON escapes, reaches the cap before formTextMax does.
	formBodyMax = 16 << 10
	formTextMax = 16 << 10
	// formMessageMax cuts the sentence a form answers with.
	formMessageMax = 512

	formInvalid  = "invalid action values"
	formFailed   = "action failed"
	formCanceled = "request canceled; the action may still complete, so do not retry blindly"
)

type fieldKind int

const (
	fieldInteger fieldKind = iota + 1
	fieldBoolean
	fieldText
	fieldChoice
)

// formField declares one input. Only the members of its kind are read.
type formField struct {
	name     string
	kind     fieldKind
	min, max int64
	maxLen   int
	// choices lists the values a fieldChoice may take, read at each
	// submission so a device that has left is refused.
	choices func() []string
}

// formValues are a validated submission, keyed by field name. Reading a
// field as a kind it was not declared as panics, which fails the request
// loudly instead of reading as a zero.
type formValues map[string]any

func (v formValues) integer(name string) int64 { return v[name].(int64) }
func (v formValues) boolean(name string) bool  { return v[name].(bool) }
func (v formValues) text(name string) string   { return v[name].(string) }

// fieldError is a refused submission. field names the declared field the
// refusal is about, or is empty when the body as a whole is refused. It
// never carries a submitted value.
type fieldError struct{ field string }

func (e *fieldError) Error() string { return formInvalid }

// formRefusal is the error a route's run returns for a refusal whose reason
// the operator should read, such as "publishing is off".
type formRefusal struct{ reason string }

func (e *formRefusal) Error() string { return "adminui: form refused: " + e.reason }

// formRoute is one POST route of a page: the fields it takes, and what it
// does with a validated submission. remote is the caller's address, which
// the sender records as who made the change.
type formRoute struct {
	fields []formField
	run    func(ctx context.Context, remote string, v formValues) (string, error)
}

// formPage is a page panel that serves its two files, the shared renderer
// and style sheet, a data route and its POST routes. The plane mounts it
// under /apps/<id>/ behind its own authentication and strips that prefix.
func formPage(id string, files fs.FS, html, script string, data func() any, routes map[string]formRoute) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", pageFile(files, html, "text/html; charset=utf-8"))
	mux.Handle("GET /"+script, pageFile(files, script, "text/javascript; charset=utf-8"))
	mux.Handle("GET /sections.js", pageFile(files, "sections.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /style.css", pageFile(files, "style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /data", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, data())
	})
	for name, route := range routes {
		mux.Handle("POST /"+name, formHandler(id, name, route))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", pageCSP)
		mux.ServeHTTP(w, r)
	})
}

// formHandler answers one POST route. The checks run cheapest and most
// decisive first, and every refusal leaves one log line that names the page,
// the route and the caller, never a submitted value.
func formHandler(page, name string, route formRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := remoteFrom(r.Context())
		refuse := func(status int, reason string, body any) {
			log.Printf("adminui: page %s route %s: refused reason=%s status=%d remote=%s", page, name, reason, status, remote)
			writeJSON(w, status, body)
		}
		type message struct {
			Error string `json:"error"`
		}

		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			refuse(http.StatusUnsupportedMediaType, "content-type", message{"unsupported content type"})
			return
		}
		body, tooBig, err := readFormBody(w, r)
		switch {
		case tooBig:
			refuse(http.StatusRequestEntityTooLarge, "body-too-large", message{"request body too large"})
			return
		case err != nil:
			refuse(http.StatusBadRequest, "body-unreadable", message{"request body unreadable"})
			return
		}
		vals, err := parseForm(body, route.fields)
		if err != nil {
			var fe *fieldError
			errors.As(err, &fe)
			refuse(http.StatusBadRequest, "invalid", struct {
				Error string `json:"error"`
				Field string `json:"field,omitempty"`
			}{formInvalid, fe.field})
			return
		}

		ctx := r.Context()
		msg, err := route.run(ctx, remote, vals)
		var refusal *formRefusal
		switch {
		case ctx.Err() != nil:
			// The browser left or the server is closing; the run may have
			// finished, so the answer says the outcome is not known.
			log.Printf("adminui: page %s route %s: outcome=canceled remote=%s", page, name, remote)
			writeJSON(w, http.StatusServiceUnavailable, message{formCanceled})
		case errors.As(err, &refusal):
			log.Printf("adminui: page %s route %s: outcome=refused remote=%s", page, name, remote)
			writeJSON(w, http.StatusUnprocessableEntity, message{boundedMessage(refusal.reason)})
		case err != nil:
			// The detail can be transport text, so it is logged and not sent.
			log.Printf("adminui: page %s route %s: outcome=failed remote=%s: %v", page, name, remote, err)
			writeJSON(w, http.StatusInternalServerError, message{formFailed})
		default:
			log.Printf("adminui: page %s route %s: outcome=ok remote=%s", page, name, remote)
			writeJSON(w, http.StatusOK, struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			}{true, boundedMessage(msg)})
		}
	})
}

// readFormBody reads the whole body under the cap. A declared length over
// the cap is refused before a byte is read.
func readFormBody(w http.ResponseWriter, r *http.Request) (body []byte, tooBig bool, err error) {
	if r.ContentLength > formBodyMax {
		return nil, true, nil
	}
	body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, formBodyMax))
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		return nil, true, nil
	}
	return body, false, err
}

// boundedMessage makes a sentence safe to send: valid UTF-8, cut to
// formMessageMax at a rune boundary.
func boundedMessage(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= formMessageMax {
		return s
	}
	s = s[:formMessageMax]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// parseForm checks body against the declared fields: one JSON object
// holding exactly the declared names, each once, with a value of the
// declared kind and range. Any refusal is a *fieldError.
func parseForm(body []byte, fields []formField) (formValues, error) {
	raw, err := decodeFormObject(body)
	if err != nil || len(raw) != len(fields) {
		return nil, &fieldError{}
	}
	out := make(formValues, len(fields))
	for _, f := range fields {
		lit, ok := raw[f.name]
		if !ok {
			return nil, &fieldError{f.name}
		}
		v, ok := f.parse(lit)
		if !ok {
			return nil, &fieldError{f.name}
		}
		out[f.name] = v
	}
	return out, nil
}

// decodeFormObject reads one JSON object and nothing after it, refusing a
// duplicate key: encoding/json would keep the last silently.
func decodeFormObject(body []byte) (map[string]json.RawMessage, error) {
	bad := errors.New("not one JSON object")
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, bad
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, bad
		}
		if _, dup := out[key]; dup {
			return nil, bad
		}
		var lit json.RawMessage
		if err := dec.Decode(&lit); err != nil {
			return nil, bad
		}
		out[key] = lit
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, bad
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, bad
	}
	return out, nil
}

func (f formField) parse(lit json.RawMessage) (any, bool) {
	switch f.kind {
	case fieldInteger:
		// ParseInt on the literal refuses 1.0, 1e2 and a quoted number, which
		// a float64 round trip would accept.
		n, err := strconv.ParseInt(string(lit), 10, 64)
		return n, err == nil && n >= f.min && n <= f.max
	case fieldBoolean:
		switch string(lit) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return nil, false
	case fieldChoice, fieldText:
		var s string
		if err := json.Unmarshal(lit, &s); err != nil {
			return nil, false
		}
		if f.kind == fieldChoice {
			return s, slices.Contains(f.choices(), s)
		}
		return s, validFormText(s, f.maxLen)
	}
	return nil, false
}

// validFormText accepts 1 to maxLen bytes of valid UTF-8 with no control
// character but tab, newline and carriage return. It refuses the C0 set, DEL,
// the C1 set (U+009B is a CSI to a terminal) and the bidirectional
// embedding, override and isolate controls, which reorder text on screen: a
// text that is forwarded or read back would otherwise carry them.
func validFormText(s string, maxLen int) bool {
	if len(s) < 1 || len(s) > maxLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r < 0x20 && r != '\n' && r != '\r' && r != '\t', r >= 0x7F && r <= 0x9F,
			r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return false
		}
	}
	return true
}
