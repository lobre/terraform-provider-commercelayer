package commercelayer

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type rateLimits map[string]map[string]*rateLimit

type rateLimit struct {
	// start is the time representing the first request
	// being made for a type and and operation.
	start time.Time

	// hit the time representing the last request
	// that was made for a type and operation.
	hit time.Time

	// interval is the duration of a period in which
	// the rate limiting applies.
	interval time.Duration

	// remaining is the number of requests left
	// for a type and operation before being rate limited.
	remaining int
}

// Let’s say we have an interval of 4 seconds represented by 4 `-`.
// Here is a diagram showing 4 periods with `X` being requests made to the endpoint.
//
//	             1     2     3     4
//		  [X---][--X-][XX--][----]
//		   ^            ^
//		 start         hit
//	                   (remaining=0)
//
// The idea of the algorithm is to execute requests until one returns with
// X-Ratelimit-Remaining set at 0, meaning we are getting rate limited.
// For this example, we are rate limited if we make 2 requests in the same period.
//
// Let’s say that this happens in response of the request made at the `hit` marker.
// When this happens, we want to wait until the beginning of the next period (fourth one).
//
// To do this, we calculate the time spent between `hit` and `start`.
// This gives us 10 seconds (or 10 `-`). Now, we apply the `interval` as modulo (`10 % 4`).
// Here, the `interval` is 4 so it gives 2 as result. This corresponds to where we
// are in the current period (note that `hit` is at position 2 in the third period).
//
// To know how long we should wait, we then just need to take the `interval` and deduce
// where we are (so 2). This gives 2 steps to wait.
func (rl *rateLimits) delay(resType string, op string) time.Duration {
	if _, exists := (*rl)[resType]; !exists {
		return 0
	}

	if _, exists := (*rl)[resType][op]; !exists {
		return 0
	}

	opLimit := (*rl)[resType][op]

	if opLimit.remaining > 0 {
		return 0
	}

	interval := int(opLimit.interval.Seconds())

	secondsSinceStart := int(time.Now().Sub(opLimit.start))
	secondsSinceIntervalStart := secondsSinceStart % interval
	secondsLeftInInterval := interval - secondsSinceIntervalStart

	return time.Duration(secondsLeftInInterval) * time.Second
}

// register will store rate limiting information about about a request and
// it should be called each time a request is being made to the endpoint, so
// that we can calculate precisely when to wait due to rate limiting.
func (rl *rateLimits) register(resType string, op string, interval time.Duration, remaining int) {
	if _, exists := (*rl)[resType]; !exists {
		(*rl)[resType] = make(map[string]*rateLimit)
	}

	if _, exists := (*rl)[resType][op]; !exists {
		(*rl)[resType][op] = &rateLimit{
			start:    time.Now(),
			interval: interval,
		}
	}

	opLimits := (*rl)[resType][op]
	opLimits.hit = time.Now()
	opLimits.remaining = remaining
}

// A throttledTransport is a transport that applies rate limiting.
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
// If so, it waits for the next rate limiting period before executing the request.
// After the request, it registers the response to update rate limits parameters.
func (tt *throttledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resType, err := getResourceTypeFromURL(r.URL.Path)
	if err != nil {
		// cannot identify resource, so skip rate limiting
		return http.DefaultTransport.RoundTrip(r)
	}

	delay := tt.rateLimits.delay(resType, r.Method)
	if delay > 0 {
		// wait until next period in case of rate limiting
		time.Sleep(delay)
	}

	resp, err := tt.transport.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	remaining, err := strconv.Atoi(resp.Header.Get("X-Ratelimit-Remaining"))
	if err != nil {
		return resp, nil
	}

	interval, err := time.ParseDuration(resp.Header.Get("X-Ratelimit-Interval"))
	if err != nil {
		return resp, nil
	}

	tt.rateLimits.register(resType, r.Method, interval, remaining)

	return resp, nil
}

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
