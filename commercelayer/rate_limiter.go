package commercelayer

import (
	"net/http"
	"strconv"
	"time"
)

type ThrottledTransport struct {
	transport http.RoundTripper

	waitDuration int
	shouldWait   bool
}

func (c *ThrottledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.shouldWait {
		time.Sleep(time.Duration(c.waitDuration) * time.Second)
	}

	resp, err := c.transport.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	rateLimitRemaining := resp.Header.Get("X-Ratelimit-Remaining")
	rateLimitInterval := resp.Header.Get("X-Ratelimit-Interval")

	remaining, err := strconv.Atoi(rateLimitRemaining)
	if err != nil {
		c.shouldWait = false
	} else {
		c.shouldWait = (remaining == 0)
	}

	interval, err := strconv.Atoi(rateLimitInterval)
	if err != nil {
		c.waitDuration = 0
	} else {
		c.waitDuration = interval
	}

	return resp, nil
}

func NewThrottledTransport(transport http.RoundTripper) http.RoundTripper {
	return &ThrottledTransport{
		transport: transport,
	}
}
