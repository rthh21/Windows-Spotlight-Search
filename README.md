# Windows Spotlight Search

O implementare în stilul **macOS Spotlight (Dark Mode)** pentru Windows, formată dintr-un backend ultra-rapid în **Go** și un frontend plutitor borderless în **C# (WPF)**.

## Caracteristici

- **Backend în Go (`spotlight-backend`)**:
  - Scanare concurentă a fișierelor utilizând un *Worker Pool* cu `goroutines`, `channels` și `sync.RWMutex`.
  - Exclude folderele de sistem sau de dezvoltare (`AppData`, `node_modules`, `.git`, `.vs`, etc.).
  - Algoritm rapid de scoring & fuzzy matching pentru sortare după relevanță.
  - Server HTTP local (`http://localhost:9876/search?q=...`) cu suport de căutare instantanee în RAM.

- **Frontend în C# WPF (`SpotlightUI`)**:
  - Fereastră plutitoare, borderless, cu colțuri rotunjite, umbră și temă macOS Dark Mode.
  - Scurtătură globală **`Alt + Space`** integrată prin Win32 API (`RegisterHotKey`).
  - Căutare dinamică cu debounce de 40ms și anulare a cererilor vechi (`CancellationTokenSource`).
  - Navigare completă din tastatură (săgeți **Sus / Jos**, închidere cu **Escape** și deschidere directă a fișierului cu **Enter** prin programul asociat din Windows).

## Rulare

### 1. Pornire Backend (Go)
```bash
cd spotlight-backend
go run main.go
```

### 2. Pornire Frontend (WPF)
```bash
cd SpotlightUI
dotnet run
```
Apasă **`Alt + Space`** de oriunde în Windows pentru a deschide Spotlight.
