//go:build !windows

package mp4merge

import (
	"os"
	"reflect"
	"time"
)

func unixCreationTime(info os.FileInfo) (time.Time, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() {
		return time.Time{}, false
	}
	for _, name := range []string{"Birthtimespec", "Birthtim", "Crtime", "CreationTime"} {
		f := v.FieldByName(name)
		if !f.IsValid() {
			continue
		}
		if f.Kind() == reflect.Struct {
			sec, ok1 := unixIntField(f, "Sec", "Tv_sec", "tv_sec")
			nsec, ok2 := unixIntField(f, "Nsec", "Tv_nsec", "tv_nsec")
			if ok1 {
				return time.Unix(sec, func() int64 {
					if ok2 {
						return nsec
					}
					return 0
				}()), true
			}
		}
		if f.Kind() == reflect.Int64 || f.Kind() == reflect.Int {
			return time.Unix(f.Int(), 0), true
		}
	}
	return time.Time{}, false
}

func unixAccessTime(info os.FileInfo) (time.Time, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() {
		return time.Time{}, false
	}
	for _, name := range []string{"Atim", "Atimespec", "Atime"} {
		f := v.FieldByName(name)
		if !f.IsValid() {
			continue
		}
		if f.Kind() == reflect.Struct {
			sec, ok1 := unixIntField(f, "Sec", "Tv_sec", "tv_sec")
			nsec, ok2 := unixIntField(f, "Nsec", "Tv_nsec", "tv_nsec")
			if ok1 {
				return time.Unix(sec, func() int64 {
					if ok2 {
						return nsec
					}
					return 0
				}()), true
			}
		}
		if f.Kind() == reflect.Int64 || f.Kind() == reflect.Int {
			return time.Unix(f.Int(), 0), true
		}
	}
	return time.Time{}, false
}

func unixIntField(v reflect.Value, names ...string) (int64, bool) {
	for _, name := range names {
		f := v.FieldByName(name)
		if f.IsValid() && (f.Kind() >= reflect.Int && f.Kind() <= reflect.Int64 || f.Kind() >= reflect.Uint && f.Kind() <= reflect.Uint64) {
			if f.Kind() >= reflect.Uint && f.Kind() <= reflect.Uint64 {
				u := f.Uint()
				if u > uint64(^uint64(0)>>1) {
					return 0, false
				}
				return int64(u), true
			}
			return f.Int(), true
		}
	}
	return 0, false
}
