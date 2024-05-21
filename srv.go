package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

//go:embed frontend
var staticFiles embed.FS

type Score struct {
	PlayerName string `json:"player_name"`
	Score      *int   `json:"score,omitempty"`
	Progress   int    `json:"progress"`
}

type HighScoreServer struct {
	scores map[string]Score
	mutex  sync.Mutex
}

func (s *HighScoreServer) getScores(n int) []Score {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	sortedScores := make([]Score, 0, len(s.scores))
	for _, score := range s.scores {
		sortedScores = append(sortedScores, score)
	}

	sort.SliceStable(sortedScores, func(i, j int) bool {
		if sortedScores[i].Score == nil && sortedScores[j].Score == nil {
			return sortedScores[i].Progress < sortedScores[j].Progress
		}
		if sortedScores[i].Score == nil {
			return false
		}
		if sortedScores[j].Score == nil {
			return true
		}
		if *sortedScores[i].Score == *sortedScores[j].Score {
			return sortedScores[i].Progress < sortedScores[j].Progress
		}
		return *sortedScores[i].Score < *sortedScores[j].Score
	})

	if n > len(sortedScores) {
		n = len(sortedScores)
	}

	return sortedScores[:n]
}

func (s *HighScoreServer) addScore(w http.ResponseWriter, r *http.Request) {
	var newScore Score
	if err := json.NewDecoder(r.Body).Decode(&newScore); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	existingScore, exists := s.scores[newScore.PlayerName]
	if !exists || isNewScoreBetter(existingScore, newScore) {
		s.scores[newScore.PlayerName] = newScore
	}

	w.WriteHeader(http.StatusCreated)
}

func isNewScoreBetter(existing, new Score) bool {
	if existing.Score == nil && new.Score != nil {
		return true
	}
	if new.Score != nil && existing.Score != nil && *new.Score < *existing.Score {
		return true
	}
	if new.Score == nil && existing.Score == nil && new.Progress < existing.Progress {
		return true
	}
	return false
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
			scores := srv.getScores(10)
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

	server := &HighScoreServer{
		scores: make(map[string]Score),
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

	http.HandleFunc("/record", server.addScore)

	listener, err := net.Listen("tcp", *host)
	if err != nil {
		log.Fatal(err)
	}

	url := fmt.Sprintf("http://%v/", listener.Addr().(*net.TCPAddr))
	fmt.Printf("Serving on %v\n", url)

	panic(http.Serve(listener, nil))
}
