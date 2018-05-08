package ste

import (
	"context"
	"github.com/Azure/azure-pipeline-go/pipeline"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"
)

// XferRetryPolicy tells the pipeline what kind of retry policy to use. See the XferRetryPolicy* constants.
// Added a new retry policy and not using the existing policy azblob.zc_retry_policy.go since there are some changes
// in the retry policy.
// Retry on all the type of network errors instead of retrying only in case of temporary or timeout errors.
type XferRetryPolicy int32

const (
	// RetryPolicyExponential tells the pipeline to use an exponential back-off retry policy
	RetryPolicyExponential XferRetryPolicy = 0

	// RetryPolicyFixed tells the pipeline to use a fixed back-off retry policy
	RetryPolicyFixed XferRetryPolicy = 1
)

// XferRetryOptions configures the retry policy's behavior.
type XferRetryOptions struct {
	// Policy tells the pipeline what kind of retry policy to use. See the XferRetryPolicy* constants.\
	// A value of zero means that you accept our default policy.
	Policy XferRetryPolicy

	// MaxTries specifies the maximum number of attempts an operation will be tried before producing an error (0=default).
	// A value of zero means that you accept our default policy. A value of 1 means 1 try and no retries.
	MaxTries int32

	// TryTimeout indicates the maximum time allowed for any single try of an HTTP request.
	// A value of zero means that you accept our default timeout. NOTE: When transferring large amounts
	// of data, the default TryTimeout will probably not be sufficient. You should override this value
	// based on the bandwidth available to the host machine and proximity to the Storage service. A good
	// starting point may be something like (60 seconds per MB of anticipated-payload-size).
	TryTimeout time.Duration

	// RetryDelay specifies the amount of delay to use before retrying an operation (0=default).
	// The delay increases (exponentially or linearly) with each retry up to a maximum specified by
	// MaxRetryDelay. If you specify 0, then you must also specify 0 for MaxRetryDelay.
	RetryDelay time.Duration

	// MaxRetryDelay specifies the maximum delay allowed before retrying an operation (0=default).
	// If you specify 0, then you must also specify 0 for RetryDelay.
	MaxRetryDelay time.Duration

	// RetryReadsFromSecondaryHost specifies whether the retry policy should retry a read operation against another host.
	// If RetryReadsFromSecondaryHost is "" (the default) then operations are not retried against another host.
	// NOTE: Before setting this field, make sure you understand the issues around reading stale & potentially-inconsistent
	// data at this webpage: https://docs.microsoft.com/en-us/azure/storage/common/storage-designing-ha-apps-with-ragrs
	RetryReadsFromSecondaryHost string // Comment this our for non-Blob SDKs
}

func (o XferRetryOptions) retryReadsFromSecondaryHost() string {
	return o.RetryReadsFromSecondaryHost // This is for the Blob SDK only
	//return "" // This is for non-blob SDKs
}

func (o XferRetryOptions) defaults() XferRetryOptions {
	if o.Policy != RetryPolicyExponential && o.Policy != RetryPolicyFixed {
		panic("XferRetryPolicy must be RetryPolicyExponential or RetryPolicyFixed")
	}
	if o.MaxTries < 0 {
		panic("MaxTries must be >= 0")
	}
	if o.TryTimeout < 0 || o.RetryDelay < 0 || o.MaxRetryDelay < 0 {
		panic("TryTimeout, RetryDelay, and MaxRetryDelay must all be >= 0")
	}
	if o.RetryDelay > o.MaxRetryDelay {
		panic("RetryDelay must be <= MaxRetryDelay")
	}
	if (o.RetryDelay == 0 && o.MaxRetryDelay != 0) || (o.RetryDelay != 0 && o.MaxRetryDelay == 0) {
		panic("Both RetryDelay and MaxRetryDelay must be 0 or neither can be 0")
	}

	IfDefault := func(current *time.Duration, desired time.Duration) {
		if *current == time.Duration(0) {
			*current = desired
		}
	}

	// Set defaults if unspecified
	if o.MaxTries == 0 {
		o.MaxTries = 4
	}
	switch o.Policy {
	case RetryPolicyExponential:
		IfDefault(&o.TryTimeout, 1*time.Minute)
		IfDefault(&o.RetryDelay, 4*time.Second)
		IfDefault(&o.MaxRetryDelay, 120*time.Second)

	case RetryPolicyFixed:
		IfDefault(&o.TryTimeout, 1*time.Minute)
		IfDefault(&o.RetryDelay, 30*time.Second)
		IfDefault(&o.MaxRetryDelay, 120*time.Second)
	}
	return o
}

func (o XferRetryOptions) calcDelay(try int32) time.Duration { // try is >=1; never 0
	pow := func(number int64, exponent int32) int64 { // pow is nested helper function
		var result int64 = 1
		for n := int32(0); n < exponent; n++ {
			result *= number
		}
		return result
	}

	delay := time.Duration(0)
	switch o.Policy {
	case RetryPolicyExponential:
		delay = time.Duration(pow(2, try-1)-1) * o.RetryDelay

	case RetryPolicyFixed:
		if try > 1 { // Any try after the 1st uses the fixed delay
			delay = o.RetryDelay
		}
	}

	// Introduce some jitter:  [0.0, 1.0) / 2 = [0.0, 0.5) + 0.8 = [0.8, 1.3)
	delay = time.Duration(int64(float32(delay.Nanoseconds())*rand.Float32()/2 + 0.8)) // NOTE: We want math/rand; not crypto/rand
	if delay > o.MaxRetryDelay {
		delay = o.MaxRetryDelay
	}
	return delay
}

// NewXferRetryPolicyFactory creates a RetryPolicyFactory object configured using the specified options.
func NewXferRetryPolicyFactory(o XferRetryOptions) pipeline.Factory {
	o = o.defaults() // Force defaults to be calculated
	return pipeline.FactoryFunc(func(next pipeline.Policy, po *pipeline.PolicyOptions) pipeline.PolicyFunc {
		return func(ctx context.Context, request pipeline.Request) (response pipeline.Response, err error) {
			// Before each try, we'll select either the primary or secondary URL.
			primaryTry := int32(0) // This indicates how many tries we've attempted against the primary DC

			// We only consider retrying against a secondary if we have a read request (GET/HEAD) AND this policy has a Secondary URL it can use
			considerSecondary := (request.Method == http.MethodGet || request.Method == http.MethodHead) && o.retryReadsFromSecondaryHost() != ""

			// Exponential retry algorithm: ((2 ^ attempt) - 1) * delay * random(0.8, 1.2)
			// When to retry: connection failure or temporary/timeout. NOTE: StorageError considers HTTP 500/503 as temporary & is therefore retryable
			// If using a secondary:
			//    Even tries go against primary; odd tries go against the secondary
			//    For a primary wait ((2 ^ primaryTries - 1) * delay * random(0.8, 1.2)
			//    If secondary gets a 404, don't fail, retry but future retries are only against the primary
			//    When retrying against a secondary, ignore the retry count and wait (.1 second * random(0.8, 1.2))
			for try := int32(1); try <= o.MaxTries; try++ {
				logf("\n=====> Try=%d\n", try)

				// Determine which endpoint to try. It's primary if there is no secondary or if it is an add # attempt.
				tryingPrimary := !considerSecondary || (try%2 == 1)
				// Select the correct host and delay
				if tryingPrimary {
					primaryTry++
					delay := o.calcDelay(primaryTry)
					logf("Primary try=%d, Delay=%v\n", primaryTry, delay)
					time.Sleep(delay) // The 1st try returns 0 delay
				} else {
					delay := time.Second * time.Duration(rand.Float32()/2+0.8)
					logf("Secondary try=%d, Delay=%v\n", try-primaryTry, delay)
					time.Sleep(delay) // Delay with some jitter before trying secondary
				}

				// Clone the original request to ensure that each try starts with the original (unmutated) request.
				requestCopy := request.Copy()

				// For each try, seek to the beginning of the Body stream. We do this even for the 1st try because
				// the stream may not be at offset 0 when we first get it and we want the same behavior for the
				// 1st try as for additional tries.
				if err = requestCopy.RewindBody(); err != nil {
					panic(err)
				}
				if !tryingPrimary {
					requestCopy.Request.URL.Host = o.retryReadsFromSecondaryHost()
				}

				// Set the server-side timeout query parameter "timeout=[seconds]"
				timeout := int32(o.TryTimeout.Seconds()) // Max seconds per try
				if deadline, ok := ctx.Deadline(); ok {  // If user's ctx has a deadline, make the timeout the smaller of the two
					t := int32(deadline.Sub(time.Now()).Seconds()) // Duration from now until user's ctx reaches its deadline
					logf("MaxTryTimeout=%d secs, TimeTilDeadline=%d sec\n", timeout, t)
					if t < timeout {
						timeout = t
					}
					if timeout < 0 {
						timeout = 0 // If timeout ever goes negative, set it to zero; this happen while debugging
					}
					logf("TryTimeout adjusted to=%d sec\n", timeout)
				}
				q := requestCopy.Request.URL.Query()
				q.Set("timeout", strconv.Itoa(int(timeout+1))) // Add 1 to "round up"
				requestCopy.Request.URL.RawQuery = q.Encode()
				logf("Url=%s\n", requestCopy.Request.URL.String())

				// Set the time for this particular retry operation and then Do the operation.
				tryCtx, tryCancel := context.WithTimeout(ctx, time.Second*time.Duration(timeout))
				//requestCopy.Body = &deadlineExceededReadCloser{r: requestCopy.Request.Body}
				response, err = next.Do(tryCtx, requestCopy) // Make the request
				/*err = improveDeadlineExceeded(err)
				if err == nil {
					response.Response().Body = &deadlineExceededReadCloser{r: response.Response().Body}
				}*/
				logf("Err=%v, response=%v\n", err, response)

				action := "" // This MUST get changed within the switch code below
				switch {
				case ctx.Err() != nil:
					action = "NoRetry: Op timeout"
				case !tryingPrimary && response != nil && response.Response().StatusCode == http.StatusNotFound:
					// If attempt was against the secondary & it returned a StatusNotFound (404), then
					// the resource was not found. This may be due to replication delay. So, in this
					// case, we'll never try the secondary again for this operation.
					considerSecondary = false
					action = "Retry: Secondary URL returned 404"
				case response != nil && response.Response().StatusCode == http.StatusBadRequest:
					// If the request failed with Bad Request, then there is no need to retry since
					// the request will fail on the future retries as well.
					action = "NoRetry: bad request error"
				case err != nil:
					// NOTE: Protocol Responder returns non-nil if REST API returns invalid status code for the invoked operation
					// retry on all the network errors.
					// zc_policy_retry perform the retries on Temporary and Timeout Errors only.
					// some errors like 'connection reset by peer' or 'transport connection broken' does not implement the Temporary interface
					// but they should be retried. So redefined the retry policy for azcopy to retry for such errors as well.
					if _, ok := err.(net.Error); ok {
						action = "Retry: net.Error and Temporary() or Timeout()"
					} else {
						action = "NoRetry: unrecognized error"
					}
				default:
					action = "NoRetry: successful HTTP request" // no error
				}

				logf("Action=%s\n", action)
				// fmt.Println(action + "\n") // This is where we could log the retry operation; action is why we're retrying
				if action[0] != 'R' { // Retry only if action starts with 'R'
					if err != nil {
						tryCancel() // If we're returning an error, cancel this current/last per-retry timeout context
					} else {
						// TODO: Right now, we've decided to leak the per-try Context until the user's Context is canceled.
						// Another option is that we wrap the last per-try context in a body and overwrite the Response's Body field with our wrapper.
						// So, when the user closes the Body, the our per-try context gets closed too.
						// Another option, is that the Last Policy do this wrapping for a per-retry context (not for the user's context)
						_ = tryCancel // So, for now, we don't call cancel: cancel()
					}
					break // Don't retry
				}
				if response.Response() != nil {
					// If we're going to retry and we got a previous response, then flush its body to avoid leaking its TCP connection
					io.Copy(ioutil.Discard, response.Response().Body)
					response.Response().Body.Close()
				}
				// If retrying, cancel the current per-try timeout context
				tryCancel()
			}
			return response, err // Not retryable or too many retries; return the last response/error
		}
	})
}

// According to https://github.com/golang/go/wiki/CompilerOptimizations, the compiler will inline this method and hopefully optimize all calls to it away
var logf = func(format string, a ...interface{}) {}
