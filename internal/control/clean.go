package control

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// A member's answers are shown on the hub's pages, under the hub's
// address. html/template escapes them, but control characters and the
// Unicode bidirectional overrides survive escaping: a member could make a
// name read as another (U+202E reverses the text after it) or put terminal
// escapes into what people copy. The hub drops them from every string a
// member's controller answers; tabs and newlines stay (commit titles,
// paths, log excerpts).

// cleanRune says whether a rune is dropped from a member's strings.
func cleanRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n':
		return false
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		return true // C0, DEL, C1
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
		return true // bidirectional embeddings, overrides, isolates and marks
	}
	return false
}

// cleanString drops what cleanRune names; the string is returned as is
// when there is nothing to drop.
func cleanString(s string) string {
	if strings.IndexFunc(s, cleanRune) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if cleanRune(r) {
			return -1
		}
		return r
	}, s)
}

// cleanErr is err with a text cleanString leaves as is; errors.Is and
// errors.As still see err. An error whose text is clean is returned
// unchanged.
func cleanErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c := cleanString(msg); c != msg {
		return &cleanError{msg: c, err: err}
	}
	return err
}

type cleanError struct {
	msg string
	err error
}

func (e *cleanError) Error() string { return e.msg }
func (e *cleanError) Unwrap() error { return e.err }

// cleanStrings rewrites every string reachable from v (a pointer) in
// place: struct fields, slices, arrays, map keys and values, and pointers.
func cleanStrings(v any) {
	cleanValue(reflect.ValueOf(v), 0)
}

func cleanValue(v reflect.Value, depth int) {
	if depth > 32 || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			if v.Kind() == reflect.Interface {
				// A value in an interface cannot be set in place: rebuild it.
				e := v.Elem()
				if e.Kind() == reflect.String && v.CanSet() {
					v.Set(reflect.ValueOf(cleanString(e.String())).Convert(e.Type()))
					return
				}
			}
			cleanValue(v.Elem(), depth+1)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(cleanString(v.String()))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				cleanValue(v.Field(i), depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return // []byte: data, not text
		}
		for i := 0; i < v.Len(); i++ {
			cleanValue(v.Index(i), depth+1)
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		// Keys are text too (target and daemon names): a key that changes
		// is re-inserted under its clean form. Keys that become equal keep
		// one entry, chosen without regard to the map's order: the one
		// whose key was clean already, else the smallest original key.
		type moved struct{ from, to, val reflect.Value }
		var keys []moved
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if !e.IsValid() {
				continue // a NaN key: it cannot be read back
			}
			c := reflect.New(e.Type()).Elem()
			c.Set(e)
			cleanValue(c, depth+1)
			kc := reflect.New(k.Type()).Elem()
			kc.Set(k)
			cleanValue(kc, depth+1)
			if kc.Equal(k) || !kc.Equal(kc) {
				v.SetMapIndex(k, c)
				continue
			}
			keys = append(keys, moved{k, kc, c})
		}
		if len(keys) == 0 {
			return
		}
		slices.SortFunc(keys, func(a, b moved) int { return strings.Compare(keyText(a.from), keyText(b.from)) })
		for _, m := range keys {
			v.SetMapIndex(m.from, reflect.Value{})
		}
		for _, m := range keys {
			if !v.MapIndex(m.to).IsValid() {
				v.SetMapIndex(m.to, m.val)
			}
		}
	}
}

// keyText orders map keys for cleanValue.
func keyText(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprintf("%#v", k)
}
