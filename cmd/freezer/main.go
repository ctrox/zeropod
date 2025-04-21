package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"
)

type Freeze struct {
	LastObservation    time.Time     `json:"lastObservation"`
	LastFreezeDuration time.Duration `json:"lastFreezeDuration"`
	Data               string        `json:"data"`
}

var ballast []byte

// Freezer is helps with e2e testing migrations. It allocates the specified
// amount of memory and constantly stores a timestamp in memory. If the last
// timestamp is older than 50 Milliseconds it will detect that as a "freeze". It
// also exposes a simple HTTP API to get the last freeze and duration and an
// endpoint to set some string data, used to store aribtrary state.
func main() {
	mem := flag.Int("memory", 0, "memory to allocate in MiB")
	flag.Parse()

	allocateMemory(*mem)

	f := Freeze{
		LastObservation: time.Now(),
	}

	http.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(&f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		slog.Info("get called", "data", b)
		fmt.Fprintf(w, "%s", b)
	})

	http.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			slog.Error("reading req body", "error", err)
			return
		}
		fr := Freeze{}
		if err := json.Unmarshal(b, &fr); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			slog.Error("unmarshal req body", "error", err)
			return
		}
		f.Data = fr.Data
		slog.Info("set called", "data", b)

		freezeJSON, err := json.Marshal(&f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			slog.Error("marshal req body", "error", err)
			return
		}
		fmt.Fprintf(w, "%s", freezeJSON)
	})

	go http.ListenAndServe(":8080", nil)
	for {
		since := time.Since(f.LastObservation)
		if since > time.Millisecond*50 {
			f.LastFreezeDuration = since
			slog.Info("observed a freeze", "duration", since.String())
		}
		f.LastObservation = time.Now()
		time.Sleep(time.Millisecond)
	}
}

func allocateMemory(mem int) {
	slog.Info("allocating memory", "bytes", mem<<20)
	// we don't really care about the randomness too much, we want something
	// quick that isn't so easily compressable.
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	ballast = make([]byte, mem<<20)
	for i := range len(ballast) {
		ballast[i] = byte(r.UintN(255))
	}
	slog.Info("done allocating", "bytes", mem<<20)
}
