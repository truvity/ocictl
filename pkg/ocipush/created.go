package ocipush

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SourceDateEpochEnv is the reproducible-builds variable that, when set,
// supplies the creation time. See https://reproducible-builds.org/specs/source-date-epoch/.
const SourceDateEpochEnv = "SOURCE_DATE_EPOCH"

// CreatedFlagUsage is the usage text of the --created flag, shared by every
// command that pushes a manifest.
const CreatedFlagUsage = "Creation time, RFC 3339 (e.g. 2026-01-02T03:04:05Z); SOURCE_DATE_EPOCH wins when set; omitted when neither is given"

// ResolveCreated returns the deterministic creation time of an artifact, in
// UTC at seconds precision, never read from the wall clock:
//
//  1. SOURCE_DATE_EPOCH, when set and non-empty (decimal Unix seconds);
//  2. else flag, an RFC 3339 timestamp, when non-empty;
//  3. else the zero time, meaning "omit the annotation".
//
// A malformed value is an error even when the other source would have won, so
// a typo is never hidden.
func ResolveCreated(flag string) (time.Time, error) {
	return resolveCreated(flag, os.Getenv(SourceDateEpochEnv))
}

func resolveCreated(flag, epoch string) (time.Time, error) {
	var fromFlag, fromEnv time.Time

	if flag != "" {
		t, err := time.Parse(time.RFC3339, flag)
		if err != nil {
			return time.Time{}, fmt.Errorf("--created %q: want an RFC 3339 timestamp such as 2026-01-02T03:04:05Z: %w", flag, err)
		}

		fromFlag = t.UTC().Truncate(time.Second)
	}

	if epoch != "" {
		secs, err := strconv.ParseInt(strings.TrimSpace(epoch), 10, 64)
		if err != nil || secs < 0 {
			return time.Time{}, fmt.Errorf("%s %q: want a non-negative integer number of Unix seconds", SourceDateEpochEnv, epoch)
		}

		fromEnv = time.Unix(secs, 0).UTC()
	}

	if !fromEnv.IsZero() {
		return fromEnv, nil
	}

	return fromFlag, nil
}
