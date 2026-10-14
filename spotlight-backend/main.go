package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileItem reprezintă un fișier indexat în memorie
type FileItem struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Extension string `json:"ext"`
	NameLower string `json:"-"` // Păstrat doar în RAM pentru căutare rapidă case-insensitive
}

// SearchResult adaugă un scor pentru sortarea după relevanță
type SearchResult struct {
	FileItem
	Score int `json:"score"`
}

// Indexer gestionează lista de fișiere în siguranță între mai multe goroutines
type Indexer struct {
	mu    sync.RWMutex
	files []FileItem
}

var ignoredDirs = map[string]bool{
	"AppData": true, "node_modules": true, ".git": true, ".vs": true,
	".vscode": true, "bin": true, "obj": true, "$RECYCLE.BIN": true,
}

func NewIndexer() *Indexer {
	return &Indexer{
		files: make([]FileItem, 0, 50000), // Pre-alocăm memorie pentru performanță
	}
}

// IndexRoot scanează concurent subdirectoarele din calea dată
func (idx *Indexer) IndexRoot(rootPath string) {
	start := time.Now()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		log.Printf("Eroare la citirea %s: %v\n", rootPath, err)
		return
	}

	var wg sync.WaitGroup
	// Canal cu buffer pentru a colecta fișierele de la toți workerii fără blocaje
	fileChan := make(chan FileItem, 2000)

	// Goroutine separată (Aggregator) care citește din canal și scrie în slice-ul temporar
	var tempFiles []FileItem
	doneAggregating := make(chan struct{})
	go func() {
		for item := range fileChan {
			tempFiles = append(tempFiles, item)
		}
		close(doneAggregating)
	}()

	// Lansăm câte o goroutine pentru fiecare folder principal din rootPath
	for _, entry := range entries {
		fullPath := filepath.Join(rootPath, entry.Name())

		if entry.IsDir() {
			if ignoredDirs[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			wg.Add(1)
			go func(dirPath string) {
				defer wg.Done()
				walkDirectory(dirPath, fileChan)
			}(fullPath)
		} else {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			fileChan <- FileItem{
				Name:      entry.Name(),
				Path:      fullPath,
				Extension: ext,
				NameLower: strings.ToLower(entry.Name()),
			}
		}
	}

	// Așteptăm toți workerii să termine scanarea
	wg.Wait()
	close(fileChan)
	<-doneAggregating // Așteptăm agregatorul să termine de adăugat în slice

	// Blocăm cu Write Lock doar pentru o fracțiune de milisecundă ca să actualizăm indexul
	idx.mu.Lock()
	idx.files = tempFiles
	idx.mu.Unlock()

	fmt.Printf("[Go Indexer] Indexat %d fișiere în %v\n", len(tempFiles), time.Since(start))
}

func walkDirectory(dir string, out chan<- FileItem) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir // Ignorăm folderele fără permisiuni
		}
		if d.IsDir() {
			if ignoredDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		out <- FileItem{
			Name:      name,
			Path:      path,
			Extension: strings.ToLower(filepath.Ext(name)),
			NameLower: strings.ToLower(name),
		}
		return nil
	})
}

// Search caută în memorie și calculează un scor de relevanță (Exact / Prefix / Substring / Fuzzy)
func (idx *Indexer) Search(query string, limit int) []FileItem {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return []FileItem{}
	}

	// Read Lock: permite multiple căutări simultane
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var matches []SearchResult

	for _, f := range idx.files {
		score := scoreMatch(f.NameLower, query)
		if score > 0 {
			// Bonus mic pentru scurtături și executabile (stil aplicație launcher)
			if f.Extension == ".lnk" || f.Extension == ".exe" {
				score += 15
			}
			matches = append(matches, SearchResult{FileItem: f, Score: score})
		}
	}

	// Sortăm descrescător după scor, apoi după lungimea numelui (cele mai scurte primele)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			return len(matches[i].Name) < len(matches[i].Name)
		}
		return matches[i].Score > matches[j].Score
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}

	result := make([]FileItem, len(matches))
	for i, m := range matches {
		result[i] = m.FileItem
	}
	return result
}

// Algoritm rapid de scoring + fuzzy matching
func scoreMatch(target, query string) int {
	if target == query {
		return 100
	}
	if strings.HasPrefix(target, query) {
		return 80
	}
	if strings.Contains(target, query) {
		return 60
	}

	// Fuzzy match: verificăm dacă toate literele din query apar în ordine în target
	tIdx, qIdx := 0, 0
	for tIdx < len(target) && qIdx < len(query) {
		if target[tIdx] == query[qIdx] {
			qIdx++
		}
		tIdx++
	}
	if qIdx == len(query) {
		return 30
	}
	return 0
}

func main() {
	indexer := NewIndexer()

	// Luăm folderul utilizatorului curent (ex: C:\Users\Nume)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "C:\\"
	}

	// Lansăm indexarea în background ca serverul HTTP să pornească instant
	go indexer.IndexRoot(homeDir)

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		results := indexer.Search(q, 8) // Returnăm top 8 rezultate (stil Spotlight)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
	})

	port := ":9876"
	fmt.Printf("Serverul Go Spotlight rulează pe http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
