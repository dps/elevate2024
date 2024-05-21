package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pkg/browser"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/billing/meterevent"
)

//go:embed frontend/dist
var staticFiles embed.FS

const DECAY float64 = 1

type Srv struct {
	rwlock      *sync.RWMutex
	rate        float64
	lastUpdated float64
}

func (srv *Srv) observe(ts float64) {
	srv.rwlock.Lock()
	defer srv.rwlock.Unlock()

	srv.rate = DECAY + math.Exp(DECAY*(srv.lastUpdated-ts))*srv.rate
	if math.IsNaN(srv.rate) {
		srv.rate = 0
	}
	srv.lastUpdated = ts
}

func (srv *Srv) read(ts float64) float64 {
	srv.rwlock.RLock()
	defer srv.rwlock.RUnlock()

	r := math.Exp(DECAY*(srv.lastUpdated-ts)) * srv.rate
	if math.IsNaN(r) {
		return 0
	} else {
		return math.Round(r*10) / 10
	}
}

func (srv *Srv) stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Type")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()

	ticker := time.NewTicker(time.Millisecond * 500)
	defer ticker.Stop()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusBadRequest)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v := srv.read(now())
			_, err := fmt.Fprintf(w, "data: %v\n\n", v)
			if err != nil {
				log.Println(err)
				return
			}
			flusher.Flush()
		}
	}
}

type LoadTestDatum struct {
	Url     string
	Headers map[string]string
	Data    map[string]interface{}
}

type LoadTestData struct {
	Data        []LoadTestDatum
	Parallelism int
	RunTime     int64 // runtime in seconds
}

func (srv *Srv) runLoadTest(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var data LoadTestData
	err = json.Unmarshal(body, &data)
	if err != nil {
		http.Error(w, "Failed to parse request body", http.StatusBadRequest)
		return
	}

	if data.Parallelism < 1 {
		http.Error(w, "Parallelism must be at least 1", http.StatusBadRequest)
		return
	}

	if data.RunTime < 1 {
		http.Error(w, "Runtime must be at least 1", http.StatusBadRequest)
		return
	}

	if len(data.Data) < 1 {
		http.Error(w, "Requests must be non-empty", http.StatusBadRequest)
		return
	}

	var wg sync.WaitGroup
	for i := 0; i < data.Parallelism; i++ {
		wg.Add(1)

		go func() {
			log.Printf("Starting load test worker %v\n", i)
			defer wg.Done()

			client := &http.Client{
				Timeout: 5 * time.Second,
			}

			var startTime = time.Now().Unix()

		outer:
			for {
				for _, datum := range data.Data {
					if time.Now().Unix()-startTime > data.RunTime {
						break outer
					}
					jsonValue, _ := json.Marshal(datum.Data)

					req, err := http.NewRequest("POST", datum.Url, bytes.NewBuffer(jsonValue))
					if err != nil {
						log.Printf("Failed to marshal HTTP request: %v\n", err)
						return
					}
					req.Header.Set("Content-Type", "application/json; charset=UTF-8")
					for k, v := range datum.Headers {
						req.Header.Set(k, v)
					}
					_, err = client.Do(req)
					if err != nil {
						log.Printf("Failed to complete HTTP request: %v", err)
					} else {
						srv.observe(now())
					}
				}
			}
			log.Printf("Exiting load test worker %v\n", i)
		}()
	}

	wg.Wait()
}

func now() float64 {
	return float64(time.Now().UnixNano()) / 1.0e9
}

func stdinReader(srv *Srv) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		srv.observe(now())
	}
}

func printRate(srv *Srv) {
	for range time.Tick(time.Second) {
		log.Printf("Current rate: %v\n", srv.read(now()))
	}
}

func main() {
	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	host := flag.String("host", ":0", "host (including port) to listen on")
	flag.Parse()

	srv := Srv{rwlock: &sync.RWMutex{}, lastUpdated: now()}

	// Set up static server
	var staticFS = fs.FS(staticFiles)
	htmlContent, err := fs.Sub(staticFS, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	fs := http.FileServer(http.FS(htmlContent))
	http.Handle("/", fs)

	go stdinReader(&srv)
	go printRate(&srv)

	http.HandleFunc("/record", func(w http.ResponseWriter, r *http.Request) {
		srv.observe(now())
		fmt.Fprintf(w, "ok")
	})

	http.HandleFunc("/meter", func(w http.ResponseWriter, r *http.Request) {

		params := &stripe.BillingMeterEventParams{
			EventName: stripe.String("words_tx_v25"),
			Payload: map[string]string{
				"value":              "25",
				"stripe_customer_id": "cus_PwZHaH5nh0G5L3",
				"input_language":     "en",
				"output_language":    "es",
			},
			Identifier: stripe.String("identifier_123"),
		}
		meterevent.New(params)
	})

	http.HandleFunc("/loadtest", srv.runLoadTest)

	// Set up streaming server
	http.HandleFunc("/events", srv.stream)

	listener, err := net.Listen("tcp", *host)
	if err != nil {
		log.Fatal(err)
	}

	url := fmt.Sprintf("http://%v/", listener.Addr().(*net.TCPAddr))
	fmt.Printf("Serving on %v\n", url)
	err = browser.OpenURL(url)
	if err != nil {
		log.Printf("Failed to open browser: %v\n", err)
	}
	panic(http.Serve(listener, nil))
}
