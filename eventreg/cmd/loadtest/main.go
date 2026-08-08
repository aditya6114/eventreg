// Command loadtest is the concurrency proof for this project.
//
// THE CLAIM: "this API never double-books a seat, even when everyone hits
// register at the same instant." This program is what backs that claim up.
//
// THE EXPERIMENT: create an event with N seats, then fire W concurrent
// booking requests at it (W > N) all released at the SAME MOMENT. If the
// locking is correct:
//   - exactly N requests get 200 OK
//   - exactly W-N requests get 409 Conflict (sold out)
//   - final seat count is EXACTLY 0 — never negative, never leftover
//
// If the locking were broken, you'd see more than N successes and/or a
// negative seat count. That's an oversell, and it's the bug this whole
// project exists to prevent.
//
// This is lesson 4 (goroutines, WaitGroup, channels) pointed at lesson 8
// (the Postgres transaction + FOR UPDATE row lock) — your own concurrency
// knowledge attacking your own API.
//
// Run it with the server already running in another terminal:
//
//	go run ./cmd/loadtest
//	go run ./cmd/loadtest -seats=50 -workers=500
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type event struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Seats int    `json:"seats"`
}

// runNonce distinguishes one run of this program from the next, so repeat runs
// don't collide on the UNIQUE(email) constraint. Seconds is plenty here because
// the collisions we hit were WITHIN a run (see registerUser).
var runNonce = time.Now().Unix()

// result is one worker's outcome: either an HTTP status, or the reason the
// request never got one.
type result struct {
	status int    // 0 means the request failed before receiving a response
	errMsg string // why it failed (empty on success)
}

func main() {
	// The `flag` package parses command-line arguments. Defaults are chosen
	// so W (workers) far exceeds N (seats) — we WANT most requests to lose,
	// because losing correctly (409) is half of what we're proving.
	base := flag.String("url", "http://localhost:8082", "API base URL")
	seats := flag.Int("seats", 100, "seats the event starts with (N)")
	workers := flag.Int("workers", 300, "concurrent booking requests to fire (W)")
	flag.Parse()

	// One shared http.Client. IMPORTANT DETAIL: Go's default transport keeps
	// only 2 idle connections per host, which would quietly serialize our
	// "concurrent" requests and make a broken server look fine. We raise it
	// so the burst is genuinely parallel — otherwise we'd be testing our own
	// client's bottleneck instead of the server's locking.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *workers,
			MaxIdleConnsPerHost: *workers,
		},
	}

	// ---- SETUP: authenticate (lesson 10 made booking a protected route) ----
	//
	// EACH WORKER NEEDS ITS OWN USER (lesson 11). Booking is now idempotent per
	// (event, user): if all 300 workers shared one account, the first request
	// would book and the other 299 would be recognised as retries — we'd be
	// testing idempotency, not seat contention. Distinct users = 300 people
	// genuinely competing, which is the scenario we care about.
	organiser, err := registerUser(client, *base, -1)
	if err != nil {
		log.Fatal("register organiser: ", err)
	}

	// ---- create a fresh event so the test is repeatable ----
	ev, err := createEvent(client, *base, *seats, organiser)
	if err != nil {
		log.Fatal("create event: ", err)
	}
	fmt.Printf("created event id=%d %q with %d seats\n", ev.ID, ev.Name, ev.Seats)
	fmt.Printf("firing %d concurrent bookings (1 seat each)...\n\n", *workers)

	// ---- THE THUNDERING HERD ----
	//
	// startGate is the "starting gun". Every worker blocks on <-startGate,
	// which never yields a value — but CLOSING a channel unblocks EVERY
	// receiver at once. That's the idiom for releasing N goroutines
	// simultaneously, and it models the real scenario: 300 students all
	// clicking "register" the moment registration opens at 12:00:00.
	startGate := make(chan struct{})

	// results collects each request's outcome. It's BUFFERED with capacity =
	// workers so no goroutine ever blocks trying to report — the send always
	// succeeds immediately (lesson 4: buffered vs unbuffered).
	//
	// We carry the ERROR TEXT too, not just a status code. A first version of
	// this test reported failures as status 0 and discarded the error, which
	// told us "191 requests failed" but not WHY — useless for debugging.
	// Diagnostics you can't act on aren't diagnostics.
	results := make(chan result, *workers)

	var wg sync.WaitGroup // counts outstanding workers (lesson 4)

	// ready counts workers that have finished registering and are parked at the
	// gate. We must wait for ALL of them before firing, or early workers would
	// book while later ones are still hashing passwords — no contention at all.
	// A fixed time.Sleep can't work here: bcrypt is deliberately slow and its
	// duration varies, so we synchronize on the actual condition instead.
	var ready sync.WaitGroup
	ready.Add(*workers)

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		// `i` is passed as an argument so each goroutine gets its own copy —
		// and here it's load-bearing, not stylistic: it's what makes each
		// worker's registration email unique (see registerUser).
		go func(worker int) {
			defer wg.Done() // guaranteed to run, even on an early return

			// Register OUTSIDE the timed section — bcrypt would otherwise
			// dominate the measurement and blur the contention we're testing.
			token, err := registerUser(client, *base, worker)
			if err != nil {
				ready.Done()
				results <- result{status: 0, errMsg: "register: " + err.Error()}
				return
			}
			ready.Done() // "I'm at the line"

			<-startGate // park here until the gun fires

			status, err := bookOneSeat(client, *base, ev.ID, token)
			if err != nil {
				// Transport-level failure: no HTTP reply at all. Keep the
				// reason so we can tell a server bug from an OS/client limit.
				results <- result{status: 0, errMsg: err.Error()}
				return
			}
			results <- result{status: status}
		}(i)
	}

	fmt.Println("registering users...")
	ready.Wait() // every worker is now at the gate
	start := time.Now()
	close(startGate) // <-- the starting gun

	wg.Wait() // block until every worker has finished
	elapsed := time.Since(start)
	close(results) // safe to close now: all senders are done

	// ---- TALLY ----
	// Ranging over a CLOSED channel drains it and then stops — this is why
	// closing matters, otherwise the range would block forever.
	counts := map[int]int{}
	errCounts := map[string]int{} // distinct failure reasons -> how many times
	for r := range results {
		counts[r.status]++
		if r.errMsg != "" {
			errCounts[r.errMsg]++
		}
	}

	// Lesson 11 changed the success codes: a NEW booking is 201 Created, while
	// 200 OK now means "you already had this booking" (an idempotent replay).
	// With distinct users per worker, replays should be zero.
	// Lesson 12 changed the losing case: a full event now WAITLISTS the user
	// (202 Accepted) instead of rejecting them (409). "Losers" are still
	// exactly workers-minus-seats, but they leave with a place in the queue.
	ok := counts[http.StatusCreated]          // 201 — a new booking was made
	waitlisted := counts[http.StatusAccepted] // 202 — queued, no seat yet
	replay := counts[http.StatusOK]           // 200 — idempotent, nothing changed
	conflict := counts[http.StatusConflict]   // 409 — should now be 0
	other := *workers - ok - waitlisted - replay - conflict

	// ---- VERIFY against the server's actual final state ----
	final, err := getEvent(client, *base, ev.ID)
	if err != nil {
		log.Fatal("get event: ", err)
	}

	fmt.Println("RESULTS")
	fmt.Printf("  201 Created   (booked)     : %d\n", ok)
	fmt.Printf("  202 Accepted  (waitlisted) : %d\n", waitlisted)
	if replay != 0 {
		fmt.Printf("  200 OK        (idempotent replay): %d\n", replay)
	}
	if conflict != 0 {
		fmt.Printf("  409 Conflict  (sold out)   : %d\n", conflict)
	}
	if other != 0 {
		fmt.Printf("  other/errors             : %d\n", other)
		// Print each DISTINCT failure reason once, with a count. Connection
		// errors here usually mean the CLIENT or OS hit a limit (ephemeral
		// ports, accept backlog) — not that the server answered incorrectly.
		for msg, n := range errCounts {
			fmt.Printf("      %4d x %s\n", n, msg)
		}
	}
	fmt.Printf("  duration                 : %s (%.0f req/s)\n",
		elapsed.Round(time.Millisecond),
		float64(*workers)/elapsed.Seconds())
	fmt.Printf("  seats remaining in DB    : %d\n\n", final.Seats)

	// ---- THE VERDICT ----
	//
	// Two CATEGORIES of problem, and conflating them would be a mistake:
	//
	//   CORRECTNESS  — did the API ever oversell? This is what the project
	//                  claims and what the row lock guarantees. A failure
	//                  here is a real bug.
	//   ENVIRONMENT  — did some requests fail to connect at all? That's the
	//                  client machine / OS running out of sockets or the
	//                  accept backlog overflowing. Not a logic bug, but it
	//                  does mean the test didn't fully exercise the server.
	correct := true

	// Oversell check: MORE successes than seats is the catastrophic case.
	if ok > *seats {
		fmt.Printf("FAIL (correctness): OVERSOLD — %d bookings succeeded for only %d seats\n", ok, *seats)
		correct = false
	}
	// Negative seats would mean the DB let the counter go below zero.
	if final.Seats < 0 {
		fmt.Printf("FAIL (correctness): seats went negative (%d)\n", final.Seats)
		correct = false
	}
	// Under-selling: only meaningful if every request actually reached the
	// server. If connections failed, missing bookings are explained by those.
	if ok < *seats && other == 0 {
		fmt.Printf("FAIL (correctness): only %d of %d seats were sold despite enough demand\n", ok, *seats)
		correct = false
	}

	// Everyone who didn't get a seat should have been queued — nobody is simply
	// turned away. That's the waitlist guarantee (lesson 12).
	if expectedQueued := *workers - ok; other == 0 && waitlisted != expectedQueued {
		fmt.Printf("FAIL (correctness): %d users missed a seat but only %d were waitlisted\n",
			expectedQueued, waitlisted)
		correct = false
	}

	if correct {
		fmt.Printf("PASS (correctness): %d seats, %d concurrent buyers, exactly %d winners, %d queued, 0 oversold.\n",
			*seats, *workers, ok, waitlisted)
	}

	if other != 0 {
		// Deliberately does NOT guess at the cause. An earlier version asserted
		// "this is a connection limit", which was wrong the very first time it
		// mattered — the real cause was duplicate registration emails. The
		// distinct error strings printed above are the actual evidence; read
		// those rather than trusting a canned explanation.
		fmt.Printf("\nNOTE: %d of %d workers did not produce a booking result.\n", other, *workers)
		fmt.Println("  See the error breakdown above for the actual cause. Common ones:")
		fmt.Println("    'connection refused' / timeouts -> client or OS socket limits; use fewer workers")
		fmt.Println("    '429'                           -> you hit your OWN rate limiter; raise it for the test")
		fmt.Println("    'register: ... 409'             -> duplicate test emails; a uniqueness bug in this tool")
	}

	if !correct {
		os.Exit(1) // non-zero exit = correctness failure, so CI could catch it
	}
}

// doJSON is one helper for every authenticated JSON request, so the
// Authorization header is applied consistently in a single place.
func doJSON(c *http.Client, method, url, token string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		// The Bearer scheme the RequireAuth middleware parses.
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.Do(req)
}

// registerUser creates a throwaway account and returns its JWT.
//
// THE EMAIL MUST BE UNIQUE PER WORKER **AND** PER RUN, and getting that wrong
// produced a genuinely instructive failure:
//
// The first version used only time.Now().UnixNano(). That looks unique — but
// on WINDOWS the system clock granularity is ~15ms, so 150 goroutines firing
// simultaneously all read the SAME nanosecond value, generated the SAME email,
// and 130 of 150 registrations came back 409 email-already-registered.
//
// Lesson: a timestamp is NOT a unique ID generator, especially not at high
// concurrency and especially not on a platform with a coarse clock. Combine it
// with something guaranteed distinct — here the worker index, plus a
// process-run nonce so consecutive runs don't collide either.
func registerUser(c *http.Client, base string, worker int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"email":    fmt.Sprintf("loadtest+%d-%d@example.com", runNonce, worker),
		"password": "loadtest-password",
	})
	resp, err := doJSON(c, http.MethodPost, base+"/auth/register", "", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

func createEvent(c *http.Client, base string, seats int, token string) (event, error) {
	body, _ := json.Marshal(map[string]any{
		"name":  fmt.Sprintf("Load Test %d", time.Now().Unix()),
		"seats": seats,
	})
	resp, err := doJSON(c, http.MethodPost, base+"/events", token, body)
	if err != nil {
		return event{}, err
	}
	defer resp.Body.Close() // lesson 4: always close the response body

	if resp.StatusCode != http.StatusCreated {
		return event{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var e event
	err = json.NewDecoder(resp.Body).Decode(&e)
	return e, err
}

// bookOneSeat sends one booking request and returns just the status code.
// We drain and close the body so the connection can be REUSED by the pool —
// leaking bodies exhausts connections fast under load.
func bookOneSeat(c *http.Client, base string, eventID int, token string) (int, error) {
	url := fmt.Sprintf("%s/events/%d/book", base, eventID)
	resp, err := doJSON(c, http.MethodPost, url, token, []byte(`{"seats":1}`))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// io.Discard is an io.Writer that throws everything away. Copying the
	// body into it drains the connection so keep-alive can reuse it; if you
	// close without draining, the connection often can't be reused and you
	// burn through sockets under load.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func getEvent(c *http.Client, base string, id int) (event, error) {
	resp, err := c.Get(fmt.Sprintf("%s/events/%d", base, id))
	if err != nil {
		return event{}, err
	}
	defer resp.Body.Close()
	var e event
	err = json.NewDecoder(resp.Body).Decode(&e)
	return e, err
}
