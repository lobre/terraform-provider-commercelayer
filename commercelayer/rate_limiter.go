// The goal of the rate limiter is to regulate requests. No restrictions are put in place
// until we receive instructions that we will get locked at the next request. In that occasion,
// the rate limiter will have to calculate the time until the next period, which is considered
// being when the limits are raised again.
//
// Let’s say we have periods having an interval of 4 seconds. Each second is represented by 4 `-`.
// Here is a diagram showing 4 periods with `X` being requests made to the endpoint.
//
//	             1     2     3     4
//		  [X---][--X-][XX--][----]
//		   ^            ^
//		 start         last
//	                   (rate limit)
//
// For this example, we state that we are getting rate limited if we make 2 requests in the same period.
//
// Let’s say that this happens in response of the request made at the `last` marker in the third period.
// When this happens, we want to wait until the beginning of the next period (fourth one).
//
// The `start` marker reprensents when the first request was made.
//
// To find out when is the next period, we calculate the time spent between `last` and `start`.
// This gives us 10 seconds. Now, we apply the `interval` as modulo. Here, the `interval` is 4,
// so it gives 2 as result (`10 % 4 = 2`).
//
// So we know the next period is two seconds ahead. To know when that time is, we simply have
// to take the time of `last` and add up two seconds.
//
// Before making a new request, we will just have to wait until the beginning of this new period.
//
// Note that the rate limiting happens per resource type and per operation (create, update, ...).
package commercelayer

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type rateLimit struct {
	// start is the time representing the first request
	// being made for a type and and operation.
	start time.Time

	// last the time representing the last request
	// that was made for a type and operation.
	last time.Time

	// interval is the duration of a period in which
	// the rate limiting applies.
	interval time.Duration

	// locked is true if the last request instructed that
	// the rate limit was reached.
	locked bool
}

// If we were rate limited, delay tells how long to wait before requesting again.
// Otherwise, it returns 0 meaning that there is no need to wait.
func (limit rateLimit) delay() time.Duration {
	if !limit.locked {
		return 0
	}

	interval := int(limit.interval.Seconds())

	secondsSinceStart := int(limit.last.Sub(limit.start))
	secondsSinceIntervalStart := secondsSinceStart % interval
	secondsLeftInInterval := interval - secondsSinceIntervalStart

	nextInterval := limit.last.Add(time.Duration(secondsLeftInInterval) * time.Second)

	now := time.Now()

	if now.After(nextInterval) {
		return 0
	}

	return nextInterval.Sub(now)
}

// Rate limits are per resource type and per operation.
// This map will store the current state of rate limits with the first level
// being the resource type and the second map being the operation.
type rateLimits map[string]map[string]*rateLimit

// waitForRateLimit checks if the current request is subject to rate limiting.
// If the request is within the rate limit, it returns immediately.
// If the request exceeds the rate limit, the function waits until the rate limit resets,
// ensuring that subsequent requests can be made without exceeding the limit.
func (limits *rateLimits) waitForRateLimit(resType string, op string) {
	if _, exists := (*limits)[resType]; !exists {
		return
	}

	if _, exists := (*limits)[resType][op]; !exists {
		return
	}

	limit := (*limits)[resType][op]

	delay := limit.delay()

	if delay > 0 {
		time.Sleep(delay)
	}
}

// register will store rate limiting information about about a request and
// it should be called each time a request is being made to the endpoint, so
// that we can calculate precisely when to wait due to rate limiting.
func (limits *rateLimits) register(resType string, op string, interval time.Duration, locked bool) {
	if _, exists := (*limits)[resType]; !exists {
		(*limits)[resType] = make(map[string]*rateLimit)
	}

	now := time.Now()

	if _, exists := (*limits)[resType][op]; !exists {
		(*limits)[resType][op] = &rateLimit{
			start:    now,
			interval: interval,
		}
	}

	limit := (*limits)[resType][op]
	limit.last = now
	limit.locked = locked
}

// A throttledTransport is a transport that takes rate limiting into account.
type throttledTransport struct {
	transport  http.RoundTripper
	rateLimits rateLimits
}

func newThrottledTransport(transport http.RoundTripper) http.RoundTripper {
	return &throttledTransport{
		transport:  transport,
		rateLimits: make(rateLimits),
	}
}

// RoundTrip extracts the resource type from the url path and the operation
// from the http method. Then it checks if those are currently rate limited.
// If so, it waits for the expiration of the rate limits before executing the request.
// After the request, it registers the response to update rate limits parameters.
func (tt *throttledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resType, err := getResourceTypeFromURL(r.URL.Path)
	if err != nil {
		// cannot identify resource, so skip rate limiting
		return http.DefaultTransport.RoundTrip(r)
	}

	tt.rateLimits.waitForRateLimit(resType, r.Method)

	resp, err := tt.transport.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	locked := resp.StatusCode == http.StatusTooManyRequests

	remainingHeader := resp.Header.Get("X-Ratelimit-Remaining")

	if remainingHeader != "" {
		remaining, err := strconv.Atoi(remainingHeader)
		if err != nil {
			return resp, nil
		}

		locked = remaining == 0
	}

	interval, err := time.ParseDuration(resp.Header.Get("X-Ratelimit-Interval"))
	if err != nil {
		return resp, nil
	}

	tt.rateLimits.register(resType, r.Method, interval, locked)

	return resp, nil
}

// getResourceTypeFromURL extracts the part of the url that represents
// the resource being targeted.
func getResourceTypeFromURL(urlPath string) (string, error) {
	apiIndex := strings.Index(urlPath, "/api/")
	if apiIndex == -1 {
		return "", errors.New("resource type not found in URL")
	}

	resourcePath := urlPath[apiIndex+5:]

	parts := strings.Split(resourcePath, "/")
	if len(parts) == 0 {
		return "", errors.New("invalid URL structure")
	}

	return parts[0], nil
}
