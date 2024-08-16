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
	"fmt"
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

	// only one request of each type can be made at the same
	// time to precisely enforce regulation.
	mu sync.Mutex
}

// If we were rate limited, delay tells how long to wait before requesting again.
// Otherwise, it returns 0 meaning that there is no need to wait.
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

// Rate limits are per resource type and per operation.
// This map will store the current state of rate limits with the first level
// being the resource type and the second map being the operation.
type rateLimits map[string]map[string]*rateLimit

// get return the limit correponding to the given resource and operation
// initializing it if needed.
func (limits *rateLimits) get(resType string, op string) *rateLimit {
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
	uuid := fmt.Sprintf("%d", time.Now().UnixNano())

	log.Printf("[AMER-%s] new request <%s> at: %s\n", uuid, r.Method, r.URL.Path)

	resType, err := getResourceTypeFromURL(r.URL.Path)
	if err != nil {
		// cannot identify resource, so skip rate limiting
		return http.DefaultTransport.RoundTrip(r)
	}

	log.Printf("[AMER-%s] resource type extracted: %s\n", uuid, resType)

	limit := tt.rateLimits.get(resType, r.Method)

	limit.mu.Lock()
	defer limit.mu.Unlock()

	log.Printf("[AMER-%s] limits details: start: %s\n", uuid, limit.start)
	log.Printf("[AMER-%s] limits details: last: %s\n", uuid, limit.last)
	log.Printf("[AMER-%s] limits details: interval: %s\n", uuid, limit.interval)
	log.Printf("[AMER-%s] limits details: locked: %t\n", uuid, limit.locked)

	delay := limit.delay(uuid)

	log.Printf("[AMER-%s] delay is: %s\n", uuid, delay)

	if delay > 0 {
		log.Printf("[AMER-%s] start waiting at: %s\n", uuid, time.Now())
		time.Sleep(delay)
		log.Printf("[AMER-%s] stop waiting at: %s\n", uuid, time.Now())
	}

	resp, err := tt.transport.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	locked := resp.StatusCode == http.StatusTooManyRequests

	remainingHeader := resp.Header.Get("X-Ratelimit-Remaining")

	log.Printf("[AMER-%s] response status code: %d\n", uuid, resp.StatusCode)
	log.Printf("[AMER-%s] response header remaining: %s\n", uuid, remainingHeader)

	if remainingHeader != "" {
		remaining, err := strconv.Atoi(remainingHeader)
		if err != nil {
			return resp, nil
		}

		locked = remaining == 0
	}

	log.Printf("[AMER-%s] locked retrieved from headers is: %t\n", uuid, locked)

	interval, err := time.ParseDuration(resp.Header.Get("X-Ratelimit-Interval") + "s")
	if err != nil {
		return resp, nil
	}

	log.Printf("[AMER-%s] response header interval: %s\n", uuid, interval)

	limit.last = time.Now()
	limit.interval = interval
	limit.locked = locked

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
