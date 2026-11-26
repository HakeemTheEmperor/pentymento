// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package s3

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/aws/smithy-go"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// errCCUnsupported means the storage refused If-None-Match on a PUT.
var errCCUnsupported = errors.New("the storage does not support conditional create (If-None-Match)")

// errInvalidRange is S3's 416 for a range starting at or past the end.
var errInvalidRange = errors.New("range not satisfiable")

// statusCode returns the HTTP status of an S3 error, or 0.
func statusCode(err error) int {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// mapError turns an S3 error into the backend's errors. cond says the
// request carried If-None-Match, so 412 and 409 mean the object exists
// (409: a concurrent conditional write to the same key is winning) and
// 501 means the storage does not support the condition.
func mapError(op, key string, err error, cond bool) error {
	if err == nil {
		return nil
	}
	code := ""
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	status := statusCode(err)
	switch {
	case status == http.StatusNotFound || code == "NoSuchKey" || code == "NotFound":
		return fmt.Errorf("%w: %s", backend.ErrNotFound, key)
	case cond && (status == http.StatusPreconditionFailed || status == http.StatusConflict ||
		code == "PreconditionFailed" || code == "ConditionalRequestConflict"):
		return fmt.Errorf("%w: %s", backend.ErrExists, key)
	case cond && (status == http.StatusNotImplemented || code == "NotImplemented"):
		return fmt.Errorf("%s %s: %w: %w", op, key, errCCUnsupported, err)
	case status == http.StatusRequestedRangeNotSatisfiable || code == "InvalidRange":
		return fmt.Errorf("%s %s: %w", op, key, errInvalidRange)
	}
	return fmt.Errorf("s3 %s %s: %w", op, key, err)
}

// jitterBackoff waits exponentially longer between attempts, from min
// up to max, each delay drawn at random from its upper half so that
// clients retrying together spread out (PRD 7.8).
type jitterBackoff struct{ min, max time.Duration }

func (j jitterBackoff) BackoffDelay(attempt int, _ error) (time.Duration, error) {
	d := j.max
	if attempt < 40 {
		if e := j.min << attempt; e > 0 && e < j.max {
			d = e
		}
	}
	half := d / 2
	if half < j.min {
		half = j.min
	}
	if half >= d {
		return d, nil
	}
	return half + rand.N(d-half+1), nil // #nosec G404 -- jitter, not security
}
