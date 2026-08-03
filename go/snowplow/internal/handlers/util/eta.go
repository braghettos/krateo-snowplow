package util

import "time"

func ETA(start time.Time) string {
	dur := time.Since(start)
	if dur < time.Second {
		return dur.String()
	}

	return dur.Round(time.Microsecond).String()
}
