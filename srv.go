package main

import (
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
)

//go:embed frontend
var staticFiles embed.FS

const THRESHOLD = 5
const NUM_SCORES = 20
const MIN_TIME = 15

type Token struct {
	Start int64  `json:start`
	Hmac  string `json:hmac`
}

type Score struct {
	PlayerName      string `json:"player_name"`
	Elapsed         int    `json:"elapsed,omitempty"`
	RemainingHealth int    `json:"remaining_health"`
	Token           Token  `json:"token"`
}

func scoreCmp(a Score, b Score) int {
	return cmp.Or(
		-cmp.Compare(a.Elapsed, b.Elapsed),
		cmp.Compare(a.RemainingHealth, b.RemainingHealth),
	)
}

type HighScoreServer struct {
	scores  []Score
	hmacKey []byte
	mutex   sync.Mutex
}

func (s *HighScoreServer) truncateAndGetScores(n int) []Score {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	slices.SortStableFunc(s.scores, scoreCmp)
	s.scores = s.scores[:n]

	return s.scores
}

func (s *HighScoreServer) addScore(w http.ResponseWriter, r *http.Request) {
	var newScore Score
	if err := json.NewDecoder(r.Body).Decode(&newScore); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// validate the score
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(newScore.Token.Start))

	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write(b)
	result := mac.Sum(nil)

	signature, err := base64.StdEncoding.DecodeString(newScore.Token.Hmac)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if !hmac.Equal(signature, result) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if newScore.Elapsed < MIN_TIME {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if newScore.RemainingHealth < 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	t := time.Now().Unix()

	if math.Abs(float64(t-newScore.Token.Start-int64(newScore.Elapsed))) >= THRESHOLD {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Zero out the token to save space
	newScore.Token = Token{}

	s.scores = append(s.scores, newScore)

	w.WriteHeader(http.StatusCreated)
}

func (s *HighScoreServer) getToken(w http.ResponseWriter, r *http.Request) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	t := time.Now().Unix()
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(t))

	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write(b)
	result := mac.Sum(nil)
	token := base64.StdEncoding.EncodeToString(result)

	w.WriteHeader(http.StatusCreated)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Token{
		Start: t,
		Hmac:  token,
	})
}

func (srv *HighScoreServer) stream(w http.ResponseWriter, r *http.Request) {
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
			scores := srv.truncateAndGetScores(NUM_SCORES)
			data, err := json.Marshal(scores)
			if err != nil {
				log.Println(err)
				return
			}
			_, err = fmt.Fprintf(w, "data: %s\n\n", data)
			if err != nil {
				log.Println(err)
				return
			}
			flusher.Flush()
		}
	}
}

func main() {

	host := flag.String("host", ":0", "host (including port) to listen on")
	flag.Parse()

	hmacKey := make([]byte, 16)
	_, err := rand.Read(hmacKey)
	if err != nil {
		panic(err)
	}

	server := &HighScoreServer{
		scores:  make([]Score, NUM_SCORES),
		hmacKey: hmacKey,
	}

	// Set up static server
	var staticFS = fs.FS(staticFiles)
	htmlContent, err := fs.Sub(staticFS, "frontend")
	if err != nil {
		log.Fatal(err)
	}
	fs := http.FileServer(http.FS(htmlContent))
	http.Handle("/", fs)

	// Set up streaming server
	http.HandleFunc("/events", server.stream)

	http.HandleFunc("/start", server.getToken)
	http.HandleFunc("/record", server.addScore)

	listener, err := net.Listen("tcp", *host)
	if err != nil {
		log.Fatal(err)
	}

	url := fmt.Sprintf("http://%v/", listener.Addr().(*net.TCPAddr))
	fmt.Printf("Serving on %v\n", url)

	panic(http.Serve(listener, nil))
}
