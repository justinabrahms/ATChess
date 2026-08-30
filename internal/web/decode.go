package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
)

// decode.go — one way in for JSON request bodies, so input hygiene cannot be
// forgotten at the tenth call site.
//
// WHY THIS EXISTS. Reported 2026-08-30 by a user who could neither sign in nor
// issue a challenge. The server log:
//
//	"handle":"atchess-player1.bsky.social  "
//	"error":"invalid handle ...: label \"social  \" must contain only ASCII
//	         letters, digits and hyphens..."
//
// Two trailing spaces, from pasting a handle into a text field. The handle
// itself is perfectly valid and resolves. Nothing in the product is at fault
// except that it took the bytes literally.
//
// There were nine separate `json.NewDecoder(r.Body).Decode(&req)` call sites.
// Fixing the two that had been reported would have left seven, and the next
// person to add a handler would have made it eight. So trimming happens here,
// once, on the way in — TestNoHandlerDecodesDirectly fails the build if a new
// handler bypasses it.
//
// WHAT IS NOT TRIMMED. A field tagged `trim:"-"` is left exactly as sent.
// AuthRequest.Password is the one that matters: a password may legitimately
// end in a space, and silently altering a credential turns a working login
// into an authentication failure with no visible cause — the same class of bug
// this file exists to fix, pointed the other way.

// decodeJSONBody decodes r's JSON body into dst and trims whitespace from
// every string it contains, except fields tagged `trim:"-"`.
func decodeJSONBody(r *http.Request, dst any) error {
	if r == nil || r.Body == nil {
		return errors.New("no request body")
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return err
	}
	trimStrings(reflect.ValueOf(dst))
	return nil
}

// trimStrings walks v and trims every settable string it reaches. Structs,
// pointers, slices, arrays and maps are all followed, because a handle can
// arrive nested just as easily as at the top level.
func trimStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			trimStrings(v.Elem())
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			// Unexported fields cannot be set, and reaching into them would
			// panic rather than fail politely.
			if !t.Field(i).IsExported() {
				continue
			}
			if t.Field(i).Tag.Get("trim") == "-" {
				continue
			}
			trimStrings(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			trimStrings(v.Index(i))
		}
	case reflect.Map:
		// Map values are not addressable, so each is trimmed via a copy and
		// written back.
		for _, k := range v.MapKeys() {
			mv := v.MapIndex(k)
			if mv.Kind() == reflect.String {
				v.SetMapIndex(k, reflect.ValueOf(strings.TrimSpace(mv.String())))
				continue
			}
			if mv.Kind() == reflect.Interface || mv.Kind() == reflect.Pointer {
				trimStrings(mv)
			}
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(strings.TrimSpace(v.String()))
		}
	}
}

// clientError reports whether err is the caller's fault rather than ours.
//
// A handle that does not exist, or is malformed, is a 400. It was a 500, which
// tells the user "something broke here, try again later" about a typo they
// could fix in two seconds — and tells whoever is watching error rates that
// the server is failing when it is working correctly. Neither audience is
// served by the wrong number.
func clientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"invalid handle",
		"failed to resolve handle",
		"unable to resolve handle",
		"handle not found",
		"invalid did",
		"must contain only ascii",
		"invalid request",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// statusForError picks 400 or 500 for err, defaulting to 500 — an unrecognised
// failure is ours until shown otherwise.
func statusForError(err error) int {
	if clientError(err) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
