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
// There are two different types of limits in Commerce Layer: average and burst. Average is
// supposed to be a limit for all requests together, whatever their resource type. And burst
// is per resource type and per operation (create, update, ...). To simplify the algorithm,
// and avoid having to differenciate those two limits, we will take the worst case and always
//
// # This above strategy works when we have the rate limiting information in previous requests before
//
// Note that the rate limiting happens per resource type and per operation (create, update, ...).
package commercelayer

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

// print values of the current limit.
func (limit rateLimit) print(w io.Writer, uuid string) {
	fmt.Fprintf(w, "[AMER-%s] limits details: start: %s\n", uuid, limit.start)
	fmt.Fprintf(w, "[AMER-%s] limits details: last: %s\n", uuid, limit.last)
	fmt.Fprintf(w, "[AMER-%s] limits details: interval: %s\n", uuid, limit.interval)
	fmt.Fprintf(w, "[AMER-%s] limits details: locked: %t\n", uuid, limit.locked)
}

// If we were rate limited, delay tells how long to wait before requesting again.
// Otherwise, it returns 0 meaning that there is no need to wait.
//
// This method does not modify the receiver so it is passed by value.
func (limit rateLimit) delay(uuid string) time.Duration {
	if !limit.locked {
		return 0
	}

	if limit.interval == 0 || limit.last.IsZero() {
		return 0
	}

	interval := int(limit.interval.Seconds())
	log.Printf("[AMER-%s] delay: interval is: %d\n", uuid, interval)

	secondsSinceStart := int(limit.last.Sub(limit.start).Seconds())
	log.Printf("[AMER-%s] delay: secondsSinceStart is: %d\n", uuid, secondsSinceStart)

	secondsSinceIntervalStart := secondsSinceStart % interval
	log.Printf("[AMER-%s] delay: secondsSinceIntervalStart is: %d\n", uuid, secondsSinceIntervalStart)

	secondsLeftInInterval := interval - secondsSinceIntervalStart
	log.Printf("[AMER-%s] delay: secondsLeftInInterval is: %d\n", uuid, secondsLeftInInterval)

	nextInterval := limit.last.Add(time.Duration(secondsLeftInInterval) * time.Second)
	log.Printf("[AMER-%s] delay: nextInterval is: %s\n", uuid, nextInterval)

	now := time.Now()
	log.Printf("[AMER-%s] delay: now is: %s\n", uuid, now)

	if now.After(nextInterval) {
		return 0
	}

	delay := nextInterval.Sub(now)
	log.Printf("[AMER-%s] delay: final delay is: %s\n", uuid, delay)
	return delay
}

// Burst rate limits are per resource type and per operation.
// This map will store the current state of rate limits with the first level
// being the resource type and the second map being the operation.
type burstRateLimits map[string]map[string]*rateLimit

// get return the burst limit correponding to the given resource and operation
// initializing it if needed.
func (limits *burstRateLimits) get(resType string, op string) *rateLimit {
	if _, exists := (*limits)[resType]; !exists {
		(*limits)[resType] = make(map[string]*rateLimit)
	}

	if _, exists := (*limits)[resType][op]; !exists {
		(*limits)[resType][op] = &rateLimit{
			start:  time.Now(),
			locked: false, // unlocked initially
		}
	}

	return (*limits)[resType][op]
}

// A throttledTransport is a transport that takes rate limiting into account.
type throttledTransport struct {
	transport http.RoundTripper

	averageRateLimit *rateLimit
	burstRateLimits  burstRateLimits

	mu sync.Mutex
}

// newThrottledTransport initializes the throttled transport.
func newThrottledTransport(transport http.RoundTripper) http.RoundTripper {
	return &throttledTransport{
		transport: transport,

		averageRateLimit: &rateLimit{},
		burstRateLimits:  make(burstRateLimits),
	}
}

// wait will wait the correct amount of time to skip rate limits, being burst or average.
func (tt *throttledTransport) wait(uuid string, resType string, op string) {
	log.Printf("[AMER-%s] information for average limit\n", uuid)
	tt.averageRateLimit.print(log.Writer(), uuid)

	delay := tt.averageRateLimit.delay(uuid)
	log.Printf("[AMER-%s] delay for average limit is: %s\n", uuid, delay)

	if delay > 0 {
		log.Printf("[AMER-%s] start waiting for average limit at: %s\n", uuid, time.Now())
		time.Sleep(delay)
		log.Printf("[AMER-%s] stop waiting for average limit at: %s\n", uuid, time.Now())

		// unlock as we waited
		tt.averageRateLimit.locked = false
	}

	burstLimit := tt.burstRateLimits.get(resType, op)

	log.Printf("[AMER-%s] information for burst limit\n", uuid)
	burstLimit.print(log.Writer(), uuid)

	delay = burstLimit.delay(uuid)
	log.Printf("[AMER-%s] delay for burst limit is: %s\n", uuid, delay)

	if delay > 0 {
		log.Printf("[AMER-%s] start waiting for burst limit at: %s\n", uuid, time.Now())
		time.Sleep(delay)
		log.Printf("[AMER-%s] stop waiting for burst limit at: %s\n", uuid, time.Now())

		// unlock as we waited
		burstLimit.locked = false
	}
}

// register will record the rate limit retrieved from the response taking into account the correct limit type.
func (tt *throttledTransport) register(resp *http.Response, uuid string, resType string, op string) {
	locked, interval, err := extractFromHeaders(resp)
	if err != nil {
		// cannot find rate limiting info, skip
		return
	}

	log.Printf("[AMER-%s] locked extracted from headers is: %t\n", uuid, locked)
	log.Printf("[AMER-%s] interval extracted from headers: %s\n", uuid, interval)

	switch interval {
	case 60 * time.Second:
		tt.averageRateLimit.last = time.Now()
		tt.averageRateLimit.locked = locked
		tt.averageRateLimit.interval = interval

	case 10 * time.Second:
		burstLimit := tt.burstRateLimits.get(resType, op)

		burstLimit.last = time.Now()
		burstLimit.locked = locked
		burstLimit.interval = interval

	default:
		// limit type not detected, skip
		return
	}
}

// RoundTrip extracts the resource type from the url path and the operation
// from the http method. Then it checks if those are currently rate limited.
// If so, it waits for the expiration of the rate limits before executing the request.
// After the request, it registers the response to update rate limits parameters.
func (tt *throttledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	uuid := fmt.Sprintf("%d", time.Now().UnixNano())

	log.Printf("[AMER-%s] new request <%s> at: %s\n", uuid, r.Method, r.URL.Path)

	// as the response of a request contains information for the next request,
	// we have to introduce contention and process requests one by one
	tt.mu.Lock()
	defer tt.mu.Unlock()

	resType, err := getResourceTypeFromURL(r.URL.Path)
	if err != nil {
		// cannot identify resource, so skip rate limiting
		return http.DefaultTransport.RoundTrip(r)
	}

	var resp *http.Response

	for {
		tt.wait(uuid, resType, r.Method)

		resp, err = tt.transport.RoundTrip(r)
		if err != nil {
			return nil, err
		}

		log.Printf("[AMER-%s] response status code: %d\n", uuid, resp.StatusCode)

		tt.register(resp, uuid, resType, r.Method)

		if resp.StatusCode != http.StatusTooManyRequests {
			break
		}
	}

	log.Printf("[AMER-%s] end of request\n", uuid)

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

// extractFromHeaders will extract the rate limiting information from the response headers.
func extractFromHeaders(resp *http.Response) (locked bool, interval time.Duration, err error) {
	remainingHeader := resp.Header.Get("X-Ratelimit-Remaining")

	if remainingHeader != "" {
		remaining, err := strconv.Atoi(remainingHeader)
		if err != nil {
			return false, 0, err
		}

		locked = remaining == 0
	}

	interval, err = time.ParseDuration(resp.Header.Get("X-Ratelimit-Interval") + "s")
	if err != nil {
		return false, 0, err
	}

	return
}
