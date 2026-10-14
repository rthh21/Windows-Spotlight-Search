# Windows Spotlight Search

A high-performance **macOS Spotlight (Dark Mode)** search experience for Windows. Built with an ultra-fast **Go** backend (featuring direct NTFS MFT indexing and real-time USN Journal change monitoring) and a lightweight, floating borderless **C# (.NET / WPF)** frontend communicating over a persistent **TCP Socket (< 1ms)**.

---

## Highlights & Features

### 🖥️ Native macOS Design & Fluidity
- **macOS Floating Frosted Capsule**: 72px pill-shaped search bar with specular rim highlights, frosted glass blur, and ambient glow.
- **Glowing Pulsing Caret**: Custom glowing vertical caret that follows text input smoothly.
- **Smart Multi-Monitor Detection**: Automatically spawns and centers the Spotlight window on whichever monitor the mouse cursor is currently positioned on.
- **Ultra-Snappy & Zero Lag**: Instantaneous UI response with zero layout sluggishness, virtualized smooth-scrolling native lists, and background icon preloading.
- **Clean & Distraction-Free**: Streamlined interface without cluttered status bars or return badges.

### 🎯 Comprehensive Application & File Search
- **iOS / macOS Spotlight & PowerToys Run Matching**: Intelligently categorizes and ranks results into:
  - `Top Hit`: Best matching application or action.
  - `Applications`: Win32 desktop apps, Windows Store / UWP apps, Steam games, and Epic Games launcher titles.
  - `Folders`: Direct directory matching.
  - `Files`: File indexing across all fixed NTFS drives.
  - `Actions`: Quick system actions and direct URL navigation.
  - `Web Search`: Instant fallback to Google search.
- **High-Resolution Icon Extraction**:
  - **UWP / Windows Store Apps**: COM `IShellItemImageFactory` extraction from `shell:AppsFolder`.
  - **Steam & Epic Games Shortcuts (`.url`)**: Native parser extracting high-resolution `.ico` and launcher icons directly.
  - **Windows Shortcuts (`.lnk`) & Executables (`.exe`)**: Shell API and associated icon extraction.
  - **File Type Icons**: Dynamic Windows Shell icons for documents, media, code, and folders.
- **Mathematical Calculator (AST)**:
  - Instant arithmetic calculation supporting parentheses, powers (`^`), and decimals (e.g. `= 12.5 * 4`, `= 2^10`, `= (100+25)/5`).
  - Press <kbd>Enter</kbd> to immediately copy the calculated result to your clipboard.
- **System Actions**:
  - `lock`: Lock Windows workstation.
  - `sleep`: Put PC into sleep mode.
  - `restart`: Restart computer.
  - `shutdown`: Power off computer.
  - `recycle` / `emptybin`: Empty the Recycle Bin.
  - `ip`: Query local network IP address and copy to clipboard on <kbd>Enter</kbd>.
  - `cmd` / `wt`: Open Command Prompt or Windows Terminal.
  - `:settings`: Open the Spotlight shortcut and command guide overlay.
- **Extension Filters**: Search specific file types directly (e.g. `ext:pdf report`, `ext:png screenshot`).

### ⚡ Blazing-Fast Go Backend (`spotlight-backend`)
- **Direct NTFS MFT (Master File Table) Enumeration**: Scans entire drives in seconds via low-level Windows `FSCTL_ENUM_USN_DATA`.
- **Live USN Journal Monitoring**: Tracks filesystem changes in real time via `FSCTL_READ_USN_JOURNAL`, giving instant $\mathcal{O}(1)$ updates for file creations, renames, and deletions without rescan overhead.
- **Zero-Pointer POD Structs (`Entry`)**: 24-byte compact Plain Old Data entries marked as `noscan` by Go's runtime, achieving 0% GC overhead and zero heap fragmentation.
- **Zero-Allocation Byte Arena**: Filenames stored in a continuous memory arena, eliminating millions of small heap allocations.
- **Hardware POPCNT & Bitmask Filtering**: 64-bit character mask filtering combined with Damerau-Levenshtein typo tolerance.
- **Persistent TCP Socket**: Real-time communication on `127.0.0.1:49152` with disabled Nagle algorithm (`TCP_NODELAY`), plus HTTP fallback on `127.0.0.1:9876`.
- **Silent Background Execution**: Runs without a console window via `-ldflags "-H=windowsgui"`.

---

## Keyboard Shortcuts

| Shortcut | Action |
| :--- | :--- |
| <kbd>Alt</kbd> + <kbd>Space</kbd> | Open or hide Spotlight on the active screen |
| <kbd>Ctrl</kbd> + <kbd>Space</kbd> | Alternative global toggle shortcut |
| <kbd>Esc</kbd> | Close Spotlight or dismiss Settings overlay |
| <kbd>↑</kbd> / <kbd>↓</kbd> | Navigate search results |
| <kbd>Enter</kbd> | Launch selected item / copy math result or IP address |
| Right-Click | Context menu: *Open File Location* or *Copy Full Path* |

---

## Getting Started

### Prerequisites
- **Windows 10 / 11** (64-bit)
- **.NET 10.0 SDK** (or .NET 8.0+)
- **Go 1.21+**
- Administrator privileges (required on first launch for direct NTFS MFT volume access)

### Quick Start
To build, restart, and launch both backend and frontend seamlessly:
```powershell
.\restart.ps1
```
*(or run `restart.bat`)*

### Manual Build & Run

#### 1. Backend (Go)
```bash
cd spotlight-backend
go build -ldflags "-H=windowsgui" -o spotlight-backend.exe
start spotlight-backend.exe
```

#### 2. Frontend (WPF)
```bash
cd SpotlightUI
dotnet run -c Release
```

Press <kbd>Alt</kbd> + <kbd>Space</kbd> from anywhere in Windows to start searching!

---

## License

This project is licensed under the terms of the **GNU Affero General Public License v3.0 (AGPL-3.0)**. See the [LICENSE](LICENSE) file for complete details.
