package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"math/bits"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// Windows API constants for NTFS MFT, USN Journal, Registry and Windows Service
const (
	FSCTL_QUERY_USN_JOURNAL = 0x000900f4
	FSCTL_ENUM_USN_DATA     = 0x000900b3
	FSCTL_READ_USN_JOURNAL  = 0x000900bb

	FILE_ATTRIBUTE_DIRECTORY = 0x00000010
	FILE_ATTRIBUTE_SYSTEM    = 0x00000004

	// USN Reason masks for live O(1) in-memory updates
	USN_REASON_FILE_CREATE     = 0x00000100
	USN_REASON_FILE_DELETE     = 0x00000200
	USN_REASON_RENAME_OLD_NAME = 0x00001000
	USN_REASON_RENAME_NEW_NAME = 0x00002000
	USN_REASON_CLOSE           = 0x80000000

	DRIVE_FIXED = 3

	// Windows Registry constants
	HKEY_LOCAL_MACHINE = 0x80000002
	HKEY_CURRENT_USER  = 0x80000001
	KEY_READ           = 0x20019

	// Windows Service constants (runs under SYSTEM at boot without UAC)
	SERVICE_WIN32_OWN_PROCESS = 0x00000010
	SERVICE_ACCEPT_STOP       = 0x00000001
	SERVICE_CONTROL_STOP      = 0x00000001
	SERVICE_START_PENDING     = 0x00000002
	SERVICE_RUNNING           = 0x00000004
	SERVICE_STOPPED           = 0x00000001
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives     = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW        = kernel32.NewProc("GetDriveTypeW")
	procGetVolumeInformation = kernel32.NewProc("GetVolumeInformationW")
	procGetConsoleWindow     = kernel32.NewProc("GetConsoleWindow")
	procSetProcessWorkingSet = kernel32.NewProc("SetProcessWorkingSetSize")

	user32         = syscall.NewLazyDLL("user32.dll")
	procShowWindow = user32.NewProc("ShowWindow")

	advapi32                       = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW              = advapi32.NewProc("RegOpenKeyExW")
	procRegEnumKeyExW              = advapi32.NewProc("RegEnumKeyExW")
	procRegQueryValueExW           = advapi32.NewProc("RegQueryValueExW")
	procRegCloseKey                = advapi32.NewProc("RegCloseKey")
	procStartServiceCtrlDispatcher = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandler = advapi32.NewProc("RegisterServiceCtrlHandlerW")
	procSetServiceStatus           = advapi32.NewProc("SetServiceStatus")
)

// Native C structures for DeviceIoControl syscalls
type usnJournalData struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

type mftEnumDataV0 struct {
	StartFileReferenceNumber uint64
	LowUsn                   int64
	HighUsn                  int64
}

type readUsnJournalDataV0 struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
}

const (
	parentFRNMask    = uint64(0x0000FFFFFFFFFFFF)
	entryFlagIsDir   = uint64(1) << 56
	entryFlagDeleted = uint64(1) << 57
)

func makeParentField(parentFRN uint64, drive byte, isDir, deleted bool) uint64 {
	val := parentFRN & parentFRNMask
	val |= uint64(drive) << 48
	if isDir {
		val |= entryFlagIsDir
	}
	if deleted {
		val |= entryFlagDeleted
	}
	return val
}

// Entry is a compact 24-byte POD (Plain Old Data) struct with ZERO POINTERS.
// Because it contains no pointers (such as string or *T), Go's GC marks []Entry
// as noscan (0% GC scanning overhead, 0 heap fragmentation).
type Entry struct {
	ParentFRN uint64 // 8 bytes: bits 0-47 = Parent FRN, bits 48-55 = Drive letter, bit 56 = IsDir, bit 57 = Deleted
	CharMask  uint64 // 8 bytes: 64-bit mask for instant POPCNT hardware bitmask filtering
	NameOff   uint32 // 4 bytes: Offset in global name arena (Engine.nameArena)
	NameLen   uint16 // 2 bytes: Name length in bytes
	_pad      uint16 // 2 bytes: Alignment padding to 24 bytes
}

func (e Entry) GetParentFRN() uint64 {
	return e.ParentFRN & parentFRNMask
}

func (e Entry) DriveByte() byte {
	return byte(e.ParentFRN >> 48)
}

func (e Entry) Drive() string {
	d := e.DriveByte()
	if d == 0 {
		return "C:"
	}
	return string([]byte{d, ':'})
}

func (e Entry) IsDir() bool {
	return (e.ParentFRN & entryFlagIsDir) != 0
}

func (e Entry) Deleted() bool {
	return (e.ParentFRN & entryFlagDeleted) != 0
}

func (e *Entry) SetDeleted(del bool) {
	if del {
		e.ParentFRN |= entryFlagDeleted
	} else {
		e.ParentFRN &^= entryFlagDeleted
	}
}

// CatalogApp represents an application from Start Menu, Desktop, or Registry App Paths
type CatalogApp struct {
	Name     string
	Path     string
	CharMask uint64
}

// FileItem is the JSON structure returned to C# (includes Category for grouping)
type FileItem struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Extension string `json:"ext"`
	Category  string `json:"category,omitempty"`
	Score     int    `json:"score,omitempty"`
}

// TCPResponse provides query latency and result items over persistent TCP socket
type TCPResponse struct {
	Query     string     `json:"query"`
	LatencyMs float64    `json:"latency_ms"`
	Count     int        `json:"count"`
	Results   []FileItem `json:"results"`
}

type ScoredEntry struct {
	Index int
	Score int
}

// VolumeIndex maintains O(1) file entry lookup per NTFS partition (C: - Z:)
type VolumeIndex struct {
	Drive     string
	JournalID uint64
	NextUsn   int64
	ByFRN     map[uint64]int32
}

type Engine struct {
	mu        sync.RWMutex
	entries   []Entry
	nameArena []byte // Continuous byte arena for all file names (zero heap string allocations)
	volumes   map[string]*VolumeIndex
	apps      []CatalogApp
}

func isIgnoredFolderName(name string) bool {
	return equalFoldASCII(name, "$recycle.bin") ||
		equalFoldASCII(name, "system volume information") ||
		equalFoldASCII(name, "windows") ||
		equalFoldASCII(name, "node_modules") ||
		equalFoldASCII(name, ".git") ||
		equalFoldASCII(name, ".vs") ||
		equalFoldASCII(name, "obj") ||
		equalFoldASCII(name, "bin") ||
		equalFoldASCII(name, "winsxs") ||
		equalFoldASCII(name, "package cache") ||
		equalFoldASCII(name, "temp") ||
		equalFoldASCII(name, "tmp")
}

func NewEngine() *Engine {
	return &Engine{
		entries:   make([]Entry, 0, 250000),
		nameArena: make([]byte, 0, 8*1024*1024), // 8 MB preallocated for file names
		volumes:   make(map[string]*VolumeIndex),
		apps:      make([]CatalogApp, 0, 512),
	}
}

// getName returns a zero-allocation string directly from the byte arena
func (e *Engine) getName(entry *Entry) string {
	if entry.NameLen == 0 {
		return ""
	}
	return unsafe.String(&e.nameArena[entry.NameOff], int(entry.NameLen))
}

// appendName appends a name into the continuous byte arena and returns offset and length
func (e *Engine) appendName(name string) (uint32, uint16) {
	if len(name) > 65535 {
		name = name[:65535]
	}
	off := uint32(len(e.nameArena))
	e.nameArena = append(e.nameArena, name...)
	return off, uint16(len(name))
}

// appendUTF16Name directly decodes UTF-16LE into the UTF-8 byte arena without intermediate allocations
func (e *Engine) appendUTF16Name(u16 []uint16) (uint32, uint16) {
	off := uint32(len(e.nameArena))
	var runeBuf [4]byte
	for i := 0; i < len(u16); i++ {
		r := rune(u16[i])
		if r < 0x80 {
			e.nameArena = append(e.nameArena, byte(r))
		} else {
			// UTF-16 surrogate pair
			if r >= 0xD800 && r <= 0xDBFF && i+1 < len(u16) {
				r2 := rune(u16[i+1])
				if r2 >= 0xDC00 && r2 <= 0xDFFF {
					r = ((r - 0xD800) << 10) + (r2 - 0xDC00) + 0x10000
					i++
				}
			}
			n := utf8.EncodeRune(runeBuf[:], r)
			e.nameArena = append(e.nameArena, runeBuf[:n]...)
		}
	}
	length := len(e.nameArena) - int(off)
	if length > 65535 {
		length = 65535
		e.nameArena = e.nameArena[:int(off)+65535]
	}
	return off, uint16(length)
}

// decodeUTF16ToStack decodes UTF-16 into a stack buffer for zero-allocation checks in Pass 1
func decodeUTF16ToStack(u16 []uint16, stackBuf *[256]byte) string {
	pos := 0
	var runeBuf [4]byte
	for i := 0; i < len(u16) && pos < 252; i++ {
		r := rune(u16[i])
		if r < 0x80 {
			stackBuf[pos] = byte(r)
			pos++
		} else {
			n := utf8.EncodeRune(runeBuf[:], r)
			for k := 0; k < n && pos < 255; k++ {
				stackBuf[pos] = runeBuf[k]
				pos++
			}
		}
	}
	return unsafe.String(&stackBuf[0], pos)
}

// trimMemoryWorkingSet releases unused memory back to OS (Windows Working Set)
func trimMemoryWorkingSet() {
	runtime.GC()
	debug.FreeOSMemory()
	handle, err := syscall.GetCurrentProcess()
	if err == nil {
		// Trim Windows Working Set (equivalent to EmptyWorkingSet)
		minusOne := ^uintptr(0)
		procSetProcessWorkingSet.Call(uintptr(handle), minusOne, minusOne)
	}
}

// Zero-allocation case-insensitive ASCII check directly on bits (c | 0x20)
func equalFoldASCII(s, qLower string) bool {
	if len(s) != len(qLower) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c |= 0x20
		}
		if c != qLower[i] {
			return false
		}
	}
	return true
}

func hasPrefixFold(s, qLower string) bool {
	if len(s) < len(qLower) {
		return false
	}
	for i := 0; i < len(qLower); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c |= 0x20
		}
		if c != qLower[i] {
			return false
		}
	}
	return true
}

func hasSuffixFold(s, qLower string) bool {
	if len(s) < len(qLower) {
		return false
	}
	offset := len(s) - len(qLower)
	for i := 0; i < len(qLower); i++ {
		c := s[offset+i]
		if c >= 'A' && c <= 'Z' {
			c |= 0x20
		}
		if c != qLower[i] {
			return false
		}
	}
	return true
}

func containsFold(s, qLower string) bool {
	n, m := len(s), len(qLower)
	if m == 0 {
		return true
	}
	if n < m {
		return false
	}
	limit := n - m
	for i := 0; i <= limit; i++ {
		match := true
		for j := 0; j < m; j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c |= 0x20
			}
			if c != qLower[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// computeCharMask maps string characters into a 64-bit bitmask register
func computeCharMask(s string) uint64 {
	var mask uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c |= 0x20
		}
		if c >= 'a' && c <= 'z' {
			mask |= 1 << (c - 'a')
		} else if c >= '0' && c <= '9' {
			mask |= 1 << (26 + (c - '0'))
		} else if c == '.' || c == '_' || c == '-' || c == ' ' {
			mask |= 1 << 37
		}
	}
	return mask
}

func stripExtension(name string) string {
	if idx := strings.LastIndexByte(name, '.'); idx > 0 {
		name = name[:idx]
	}
	name = strings.ReplaceAll(name, "®", "")
	name = strings.ReplaceAll(name, "™", "")
	name = strings.ReplaceAll(name, "©", "")
	return strings.TrimSpace(name)
}

// damerauLevenshtein calculates Damerau-Levenshtein edit distance with transposition without heap allocations
func damerauLevenshtein(s, t string, maxDist int) int {
	n, m := len(s), len(t)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}
	diff := n - m
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return maxDist + 1
	}
	if n > 63 {
		n = 63
	}
	if m > 63 {
		m = 63
	}

	var v0, v1, v2 [64]int
	for j := 0; j <= m; j++ {
		v1[j] = j
	}

	for i := 1; i <= n; i++ {
		v2[0] = i
		minRow := i
		c1 := s[i-1]
		if c1 >= 'A' && c1 <= 'Z' {
			c1 |= 0x20
		}

		for j := 1; j <= m; j++ {
			c2 := t[j-1]
			if c2 >= 'A' && c2 <= 'Z' {
				c2 |= 0x20
			}

			cost := 0
			if c1 != c2 {
				cost = 1
			}
			del := v1[j] + 1
			ins := v2[j-1] + 1
			sub := v1[j-1] + cost

			val := del
			if ins < val {
				val = ins
			}
			if sub < val {
				val = sub
			}

			if i > 1 && j > 1 {
				cPrev := s[i-2]
				if cPrev >= 'A' && cPrev <= 'Z' {
					cPrev |= 0x20
				}
				c2Prev := t[j-2]
				if c2Prev >= 'A' && c2Prev <= 'Z' {
					c2Prev |= 0x20
				}
				if c1 == c2Prev && cPrev == c2 {
					trans := v0[j-2] + cost
					if trans < val {
						val = trans
					}
				}
			}
			v2[j] = val
			if val < minRow {
				minRow = val
			}
		}
		if minRow > maxDist {
			return maxDist + 1
		}
		v0 = v1
		v1 = v2
	}
	return v1[m]
}

// scoreAdvanced combines exact match, prefix, POPCNT bitmask and Damerau-Levenshtein
func scoreAdvanced(target string, targetMask uint64, queryLower string, queryMask uint64, queryUniqueBits int) int {
	if equalFoldASCII(target, queryLower) {
		return 150
	}
	baseTarget := stripExtension(target)
	if equalFoldASCII(baseTarget, queryLower) {
		return 145
	}
	if hasPrefixFold(target, queryLower) {
		return 115
	}
	if containsFold(target, queryLower) {
		return 85
	}

	commonBits := bits.OnesCount64(targetMask & queryMask)
	allowedMissing := 1
	if len(queryLower) >= 6 {
		allowedMissing = 2
	}
	if commonBits+allowedMissing < queryUniqueBits {
		return 0
	}

	tIdx, qIdx := 0, 0
	for tIdx < len(target) && qIdx < len(queryLower) {
		c := target[tIdx]
		if c >= 'A' && c <= 'Z' {
			c |= 0x20
		}
		if c == queryLower[qIdx] {
			qIdx++
		}
		tIdx++
	}
	if qIdx == len(queryLower) {
		return 45
	}

	if len(queryLower) >= 4 {
		maxDist := 1
		if len(queryLower) >= 6 {
			maxDist = 2
		}

		compareStr := baseTarget
		if len(compareStr) > len(queryLower)+maxDist {
			compareStr = compareStr[:len(queryLower)]
		}

		dist := damerauLevenshtein(compareStr, queryLower, maxDist)
		if dist <= maxDist {
			return 75 - (dist * 15)
		}
	}

	return 0
}

// DetectNTFSPartitions automatically detects all fixed NTFS partitions (C: - Z:)
func DetectNTFSPartitions() []string {
	var drives []string
	bitmask, _, _ := procGetLogicalDrives.Call()

	for i := 0; i < 26; i++ {
		if bitmask&(1<<uint(i)) == 0 {
			continue
		}
		driveLetter := fmt.Sprintf("%c:", 'A'+i)
		rootPath, _ := syscall.UTF16PtrFromString(driveLetter + `\`)

		driveType, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(rootPath)))
		if driveType != DRIVE_FIXED {
			continue
		}

		var fsNameBuf [32]uint16
		ret, _, _ := procGetVolumeInformation.Call(
			uintptr(unsafe.Pointer(rootPath)),
			0, 0, 0, 0, 0,
			uintptr(unsafe.Pointer(&fsNameBuf[0])),
			uintptr(len(fsNameBuf)),
		)
		if ret != 0 {
			fsName := syscall.UTF16ToString(fsNameBuf[:])
			if strings.EqualFold(fsName, "NTFS") {
				drives = append(drives, driveLetter)
			}
		}
	}
	if len(drives) == 0 {
		return []string{"C:"}
	}
	return drives
}

// BuildAppCatalog scans Windows applications (UWP / Store / Win32 / Start Menu / Registry)
func (e *Engine) BuildAppCatalog() {
	seen := make(map[string]bool)
	var apps []CatalogApp

	addApp := func(name, fullPath string) {
		cleanName := strings.TrimSpace(stripExtension(name))
		cleanName = strings.TrimSuffix(cleanName, " - Shortcut")
		cleanName = strings.TrimSuffix(cleanName, " - Comandă rapidă")
		if cleanName == "" || len(cleanName) < 2 {
			return
		}
		lower := strings.ToLower(cleanName)
		if seen[lower] {
			return
		}
		if strings.Contains(lower, "uninstall") || strings.Contains(lower, "dezinstalare") ||
			strings.Contains(lower, "help") || strings.Contains(lower, "documentation") ||
			strings.Contains(lower, "release notes") || strings.HasSuffix(lower, ".chm") {
			return
		}
		seen[lower] = true
		apps = append(apps, CatalogApp{
			Name:     cleanName,
			Path:     fullPath,
			CharMask: computeCharMask(cleanName),
		})
	}

	// 1. Essential system applications and tools (Calculator, Settings, Terminal, etc.)
	builtins := []struct {
		Name string
		Path string
	}{
		{"Calculator", `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`},
		{"Settings", `shell:AppsFolder\windows.immersivecontrolpanel_cw5n1h2txyewy!microsoft.windows.immersivecontrolpanel`},
		{"Terminal", `shell:AppsFolder\Microsoft.WindowsTerminal_8wekyb3d8bbwe!App`},
		{"Notepad", `C:\Windows\System32\notepad.exe`},
		{"Paint", `C:\Windows\System32\mspaint.exe`},
		{"Task Manager", `C:\Windows\System32\taskmgr.exe`},
		{"Control Panel", `C:\Windows\System32\control.exe`},
		{"Command Prompt", `C:\Windows\System32\cmd.exe`},
		{"PowerShell", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`},
		{"Registry Editor", `C:\Windows\regedit.exe`},
		{"Snipping Tool", `C:\Windows\System32\SnippingTool.exe`},
		{"File Explorer", `C:\Windows\explorer.exe`},
		{"Device Manager", `C:\Windows\System32\devmgmt.msc`},
		{"Disk Management", `C:\Windows\System32\diskmgmt.msc`},
		{"Services", `C:\Windows\System32\services.msc`},
		{"Resource Monitor", `C:\Windows\System32\resmon.exe`},
		{"System Configuration", `C:\Windows\System32\msconfig.exe`},
		{"Disk Cleanup", `C:\Windows\System32\cleanmgr.exe`},
	}
	for _, b := range builtins {
		addApp(b.Name, b.Path)
	}

	// 2. Start Menu and Desktop folders (User, OneDrive, and Public)
	userProfile := os.Getenv("USERPROFILE")
	appData := os.Getenv("APPDATA")
	progData := os.Getenv("PROGRAMDATA")

	folders := []string{
		filepath.Join(appData, `Microsoft\Windows\Start Menu\Programs`),
		filepath.Join(progData, `Microsoft\Windows\Start Menu\Programs`),
		filepath.Join(userProfile, `Desktop`),
		filepath.Join(userProfile, `OneDrive`, `Desktop`),
		`C:\Users\Public\Desktop`,
	}

	for _, dir := range folders {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if ext == ".lnk" || ext == ".exe" || ext == ".url" {
				addApp(d.Name(), path)
			}
			return nil
		})
	}

	// 3. Registry App Paths
	scanRegistryAppPaths(HKEY_LOCAL_MACHINE, addApp)
	scanRegistryAppPaths(HKEY_CURRENT_USER, addApp)

	e.mu.Lock()
	e.apps = apps
	e.mu.Unlock()

	// 4. Asynchronously scan UWP / Windows Store apps from shell:AppsFolder via Get-StartApps
	go e.scanUWPApps()
}

func (e *Engine) scanUWPApps() {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	var items []struct {
		Name  string `json:"Name"`
		AppID string `json:"AppID"`
	}
	if json.Unmarshal(out, &items) != nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	seen := make(map[string]bool)
	for _, a := range e.apps {
		seen[strings.ToLower(a.Name)] = true
	}

	for _, it := range items {
		cleanName := strings.TrimSpace(stripExtension(it.Name))
		if cleanName == "" || len(cleanName) < 2 {
			continue
		}
		lower := strings.ToLower(cleanName)
		if seen[lower] {
			continue
		}
		if strings.Contains(lower, "uninstall") || strings.Contains(lower, "dezinstalare") ||
			strings.Contains(lower, "help") || strings.Contains(lower, "documentation") ||
			strings.Contains(lower, "release notes") || strings.HasSuffix(lower, ".chm") {
			continue
		}

		fullPath := it.AppID
		if !strings.HasPrefix(fullPath, "http:") && !strings.HasPrefix(fullPath, "https:") && !strings.HasPrefix(fullPath, "steam:") {
			if !filepath.IsAbs(fullPath) || strings.HasPrefix(fullPath, "{") {
				fullPath = `shell:AppsFolder\` + it.AppID
			}
		}
		seen[lower] = true
		e.apps = append(e.apps, CatalogApp{
			Name:     cleanName,
			Path:     fullPath,
			CharMask: computeCharMask(cleanName),
		})
	}
}

// matchApp performs smart application matching (macOS Spotlight / PowerToys Run style)
func matchApp(appName, query string) (int, bool) {
	nameLower := strings.ToLower(appName)
	qLower := strings.ToLower(strings.TrimSpace(query))
	if qLower == "" {
		return 0, false
	}

	// 1. Exact match
	if nameLower == qLower {
		return 350, true
	}

	// 2. Full name prefix match: e.g. "calc" -> "Calculator"
	if strings.HasPrefix(nameLower, qLower) {
		score := 300 - (len(nameLower)-len(qLower))*2
		if score < 240 {
			score = 240
		}
		return score, true
	}

	// 3. Word-prefix match: ex. "code" -> "Visual Studio Code", "edge" -> "Microsoft Edge"
	words := strings.FieldsFunc(nameLower, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '.' || r == '(' || r == ')'
	})
	for _, w := range words {
		if strings.HasPrefix(w, qLower) {
			score := 260 - (len(w)-len(qLower))*2
			if score < 210 {
				score = 210
			}
			return score, true
		}
	}

	// 4. Acronime / Inițiale cuvinte: ex. "vsc" -> "Visual Studio Code", "wt" -> "Windows Terminal"
	if len(words) > 1 && len(qLower) <= len(words) {
		var acronym strings.Builder
		for _, w := range words {
			if len(w) > 0 {
				acronym.WriteByte(w[0])
			}
		}
		acr := acronym.String()
		if strings.HasPrefix(acr, qLower) {
			return 235, true
		}
	}

	// 5. Subsequence într-un singur cuvânt: ex. "spot" -> "Spotify", "term" -> "Terminal"
	if len(qLower) >= 3 {
		for _, w := range words {
			if len(w) >= len(qLower) {
				qIdx := 0
				for i := 0; i < len(w) && qIdx < len(qLower); i++ {
					if w[i] == qLower[qIdx] {
						qIdx++
					}
				}
				if qIdx == len(qLower) {
					score := 180 - (len(w)-len(qLower))*2
					if score < 130 {
						score = 130
					}
					return score, true
				}
			}
		}
	}

	// 6. Fuzzy / Typo match cu Damerau-Levenshtein
	if len(qLower) >= 4 {
		maxDist := 1
		if len(qLower) >= 6 {
			maxDist = 2
		}
		dist := damerauLevenshtein(nameLower, qLower, maxDist)
		if dist <= maxDist {
			return 135 - dist*20, true
		}
		for _, w := range words {
			distW := damerauLevenshtein(w, qLower, maxDist)
			if distW <= maxDist {
				return 125 - distW*20, true
			}
		}
	}

	// 7. Aliasuri și Sinonime (Română / Engleză)
	aliases := map[string][]string{
		"calculator":   {"calc", "calculator", "socotit", "calcule"},
		"settings":     {"setari", "settings", "configurare", "optiuni"},
		"terminal":     {"consola", "cmd", "terminal", "powershell", "bash", "wt"},
		"photos":       {"poze", "photos", "imagini", "galerie"},
		"explorer":     {"fisiere", "explorer", "folder", "my computer"},
		"task manager": {"taskmgr", "task manager", "procese", "activitati"},
	}
	for targetApp, triggerList := range aliases {
		if strings.Contains(nameLower, targetApp) {
			for _, tr := range triggerList {
				if tr == qLower || strings.HasPrefix(tr, qLower) {
					return 270, true
				}
			}
		}
	}

	return 0, false
}

func scanRegistryAppPaths(rootKey uintptr, addApp func(name, path string)) {
	subKeyPath, _ := syscall.UTF16PtrFromString(`SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths`)
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(rootKey, uintptr(unsafe.Pointer(subKeyPath)), 0, KEY_READ, uintptr(unsafe.Pointer(&hKey)))
	if ret != 0 {
		return
	}
	defer procRegCloseKey.Call(hKey)

	for idx := uint32(0); ; idx++ {
		var nameBuf [260]uint16
		nameLen := uint32(len(nameBuf))
		ret, _, _ := procRegEnumKeyExW.Call(
			hKey,
			uintptr(idx),
			uintptr(unsafe.Pointer(&nameBuf[0])),
			uintptr(unsafe.Pointer(&nameLen)),
			0, 0, 0, 0,
		)
		if ret != 0 {
			break
		}
		exeName := syscall.UTF16ToString(nameBuf[:nameLen])

		var hSubKey uintptr
		subKeyPtr, _ := syscall.UTF16PtrFromString(exeName)
		if r, _, _ := procRegOpenKeyExW.Call(hKey, uintptr(unsafe.Pointer(subKeyPtr)), 0, KEY_READ, uintptr(unsafe.Pointer(&hSubKey))); r == 0 {
			var valBuf [520]uint16
			valBytes := uint32(len(valBuf) * 2)
			if r2, _, _ := procRegQueryValueExW.Call(hSubKey, 0, 0, 0, uintptr(unsafe.Pointer(&valBuf[0])), uintptr(unsafe.Pointer(&valBytes))); r2 == 0 {
				fullPath := strings.Trim(syscall.UTF16ToString(valBuf[:]), `" `)
				if fullPath != "" {
					addApp(exeName, fullPath)
				}
			}
			procRegCloseKey.Call(hSubKey)
		}
	}
}

// IndexAllVolumes scanează toate partițiile NTFS și eliberează memoria temporară către Windows
func (e *Engine) IndexAllVolumes() {
	e.BuildAppCatalog()
	drives := DetectNTFSPartitions()

	anySuccess := false
	for _, drive := range drives {
		if err := e.IndexMFTVolume(drive); err == nil {
			anySuccess = true
			go e.WatchUSNJournal(drive)
		}
	}

	if !anySuccess {
		home, _ := os.UserHomeDir()
		e.FallbackConcurrentWalk(home)
	}

	// Eliberăm imediat către OS toate map-urile temporare din Pass 1 și golurile de capacitate
	trimMemoryWorkingSet()
}

// IndexMFTVolume citește direct Master File Table scriind direct în Engine (fără dublarea memoriei cu localEntries)
func (e *Engine) IndexMFTVolume(drive string) error {
	volPath, err := syscall.UTF16PtrFromString(`\\.\` + drive)
	if err != nil {
		return err
	}

	handle, err := syscall.CreateFile(
		volPath,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return fmt.Errorf("acces refuzat la volumul raw %s: %w", drive, err)
	}
	defer syscall.CloseHandle(handle)

	var journalData usnJournalData
	var bytesReturned uint32

	err = syscall.DeviceIoControl(
		handle,
		FSCTL_QUERY_USN_JOURNAL,
		nil, 0,
		(*byte)(unsafe.Pointer(&journalData)),
		uint32(unsafe.Sizeof(journalData)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		return fmt.Errorf("FSCTL_QUERY_USN_JOURNAL a eșuat pe %s: %w", drive, err)
	}

	const rootFRN = 0x0000000000000005
	buf := make([]byte, 64*1024)
	var stackNameBuf [256]byte

	// ========================================================================
	// TRECEREA 1 (Pass 1): Construim arborele de directoare fără alocări de string-uri
	// ========================================================================
	enumDataPass1 := mftEnumDataV0{
		StartFileReferenceNumber: 0,
		LowUsn:                   0,
		HighUsn:                  journalData.NextUsn,
	}

	dirParents := make(map[uint64]uint64, 32000)
	ignoredFRNs := make(map[uint64]struct{}, 8000)
	dirParents[rootFRN] = 0

	for {
		err = syscall.DeviceIoControl(
			handle,
			FSCTL_ENUM_USN_DATA,
			(*byte)(unsafe.Pointer(&enumDataPass1)),
			uint32(unsafe.Sizeof(enumDataPass1)),
			&buf[0],
			uint32(len(buf)),
			&bytesReturned,
			nil,
		)
		if err != nil || bytesReturned <= 8 {
			break
		}

		enumDataPass1.StartFileReferenceNumber = binary.LittleEndian.Uint64(buf[:8])

		offset := uint32(8)
		for offset < bytesReturned {
			recordLen := binary.LittleEndian.Uint32(buf[offset : offset+4])
			if recordLen == 0 {
				break
			}

			fileAttributes := binary.LittleEndian.Uint32(buf[offset+52 : offset+56])
			if (fileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0 {
				fileNameLen := binary.LittleEndian.Uint16(buf[offset+56 : offset+58])
				fileNameOffset := binary.LittleEndian.Uint16(buf[offset+58 : offset+60])

				if fileNameLen > 0 {
					nameStart := offset + uint32(fileNameOffset)
					nameEnd := nameStart + uint32(fileNameLen)

					if nameEnd <= bytesReturned {
						frn := binary.LittleEndian.Uint64(buf[offset+8:offset+16]) & 0x0000FFFFFFFFFFFF
						parentFrn := binary.LittleEndian.Uint64(buf[offset+16:offset+24]) & 0x0000FFFFFFFFFFFF

						u16Slice := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[nameStart])), fileNameLen/2)
						name := decodeUTF16ToStack(u16Slice, &stackNameBuf)

						if (fileAttributes&FILE_ATTRIBUTE_SYSTEM) != 0 || strings.HasPrefix(name, "$") || isIgnoredFolderName(name) {
							ignoredFRNs[frn] = struct{}{}
							dirParents[frn] = parentFrn
						} else {
							dirParents[frn] = parentFrn
						}
					}
				}
			}
			offset += recordLen
		}
	}

	// Propagăm pruning-ul către toți descendenții directoarelor ignorate
	validDirCount := 0
	for frn := range dirParents {
		curr := frn
		depth := 0
		isIgnored := false
		for curr != 0 && curr != rootFRN && depth < 64 {
			if _, ok := ignoredFRNs[curr]; ok {
				ignoredFRNs[frn] = struct{}{}
				isIgnored = true
				break
			}
			p, exists := dirParents[curr]
			if !exists || p == curr {
				break
			}
			curr = p
			depth++
		}
		if !isIgnored {
			validDirCount++
		}
	}
	// Eliberăm imediat harta `dirParents` înainte de Pass 2!
	dirParents = nil

	// ========================================================================
	// TRECEREA 2 (Pass 2): Scriem direct în `e.entries` și `e.nameArena`
	// ========================================================================
	enumDataPass2 := mftEnumDataV0{
		StartFileReferenceNumber: 0,
		LowUsn:                   0,
		HighUsn:                  journalData.NextUsn,
	}

	// Estimăm mărimea pentru a evita redimensionarea inutilă a hărții
	byFRN := make(map[uint64]int32, validDirCount*4)
	driveByte := drive[0]

	e.mu.Lock()
	rootOff, rootLen := e.appendName(drive)
	rootIdx := int32(len(e.entries))
	e.entries = append(e.entries, Entry{
		ParentFRN: makeParentField(0, driveByte, true, false),
		CharMask:  computeCharMask(drive),
		NameOff:   rootOff,
		NameLen:   rootLen,
	})
	byFRN[rootFRN] = rootIdx

	for {
		err = syscall.DeviceIoControl(
			handle,
			FSCTL_ENUM_USN_DATA,
			(*byte)(unsafe.Pointer(&enumDataPass2)),
			uint32(unsafe.Sizeof(enumDataPass2)),
			&buf[0],
			uint32(len(buf)),
			&bytesReturned,
			nil,
		)
		if err != nil || bytesReturned <= 8 {
			break
		}

		enumDataPass2.StartFileReferenceNumber = binary.LittleEndian.Uint64(buf[:8])

		offset := uint32(8)
		for offset < bytesReturned {
			recordLen := binary.LittleEndian.Uint32(buf[offset : offset+4])
			if recordLen == 0 {
				break
			}

			parentFrn := binary.LittleEndian.Uint64(buf[offset+16:offset+24]) & 0x0000FFFFFFFFFFFF
			if _, ignored := ignoredFRNs[parentFrn]; ignored {
				offset += recordLen
				continue
			}

			frn := binary.LittleEndian.Uint64(buf[offset+8:offset+16]) & 0x0000FFFFFFFFFFFF
			if _, ignored := ignoredFRNs[frn]; ignored {
				offset += recordLen
				continue
			}

			fileAttributes := binary.LittleEndian.Uint32(buf[offset+52 : offset+56])
			fileNameLen := binary.LittleEndian.Uint16(buf[offset+56 : offset+58])
			fileNameOffset := binary.LittleEndian.Uint16(buf[offset+58 : offset+60])

			if (fileAttributes&FILE_ATTRIBUTE_SYSTEM) == 0 && fileNameLen > 0 {
				nameStart := offset + uint32(fileNameOffset)
				nameEnd := nameStart + uint32(fileNameLen)

				if nameEnd <= bytesReturned {
					u16Slice := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[nameStart])), fileNameLen/2)
					if len(u16Slice) > 0 && u16Slice[0] != '$' {
						isDir := (fileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0
						idx := int32(len(e.entries))
						nameOff, nameLen := e.appendUTF16Name(u16Slice)
						nameStr := unsafe.String(&e.nameArena[nameOff], int(nameLen))

						e.entries = append(e.entries, Entry{
							ParentFRN: makeParentField(parentFrn, driveByte, isDir, false),
							CharMask:  computeCharMask(nameStr),
							NameOff:   nameOff,
							NameLen:   nameLen,
						})
						byFRN[frn] = idx
					}
				}
			}
			offset += recordLen
		}
	}

	e.volumes[drive] = &VolumeIndex{
		Drive:     drive,
		JournalID: journalData.UsnJournalID,
		NextUsn:   journalData.NextUsn,
		ByFRN:     byFRN,
	}
	e.mu.Unlock()

	ignoredFRNs = nil
	return nil
}

// WatchUSNJournal ascultă în timp real modificările NTFS prin FSCTL_READ_USN_JOURNAL
func (e *Engine) WatchUSNJournal(drive string) {
	volPath, err := syscall.UTF16PtrFromString(`\\.\` + drive)
	if err != nil {
		return
	}

	handle, err := syscall.CreateFile(
		volPath,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(handle)

	e.mu.RLock()
	vol, ok := e.volumes[drive]
	if !ok {
		e.mu.RUnlock()
		return
	}
	readData := readUsnJournalDataV0{
		StartUsn:          vol.NextUsn,
		ReasonMask:        USN_REASON_FILE_CREATE | USN_REASON_FILE_DELETE | USN_REASON_RENAME_NEW_NAME | USN_REASON_RENAME_OLD_NAME | USN_REASON_CLOSE,
		ReturnOnlyOnClose: 0,
		Timeout:           0,
		BytesToWaitFor:    1,
		UsnJournalID:      vol.JournalID,
	}
	e.mu.RUnlock()

	buf := make([]byte, 16*1024)
	var bytesReturned uint32

	for {
		err := syscall.DeviceIoControl(
			handle,
			FSCTL_READ_USN_JOURNAL,
			(*byte)(unsafe.Pointer(&readData)),
			uint32(unsafe.Sizeof(readData)),
			&buf[0],
			uint32(len(buf)),
			&bytesReturned,
			nil,
		)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		if bytesReturned <= 8 {
			time.Sleep(250 * time.Millisecond)
			continue
		}

		readData.StartUsn = int64(binary.LittleEndian.Uint64(buf[:8]))

		offset := uint32(8)
		e.mu.Lock()
		for offset < bytesReturned {
			recordLen := binary.LittleEndian.Uint32(buf[offset : offset+4])
			if recordLen == 0 {
				break
			}

			frn := binary.LittleEndian.Uint64(buf[offset+8:offset+16]) & 0x0000FFFFFFFFFFFF
			parentFrn := binary.LittleEndian.Uint64(buf[offset+16:offset+24]) & 0x0000FFFFFFFFFFFF
			reason := binary.LittleEndian.Uint32(buf[offset+40 : offset+44])
			fileAttributes := binary.LittleEndian.Uint32(buf[offset+52 : offset+56])
			fileNameLen := binary.LittleEndian.Uint16(buf[offset+56 : offset+58])
			fileNameOffset := binary.LittleEndian.Uint16(buf[offset+58 : offset+60])

			nameStart := offset + uint32(fileNameOffset)
			nameEnd := nameStart + uint32(fileNameLen)

			if fileNameLen > 0 && nameEnd <= bytesReturned {
				u16Slice := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[nameStart])), fileNameLen/2)

				if len(u16Slice) > 0 && u16Slice[0] != '$' {
					if reason&USN_REASON_FILE_DELETE != 0 {
						if idx, exists := vol.ByFRN[frn]; exists {
							if int(idx) < len(e.entries) {
								e.entries[idx].SetDeleted(true)
							}
							delete(vol.ByFRN, frn)
						}
					} else if reason&(USN_REASON_FILE_CREATE|USN_REASON_RENAME_NEW_NAME) != 0 {
						isDir := (fileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0
						nameOff, nameLen := e.appendUTF16Name(u16Slice)
						nameStr := unsafe.String(&e.nameArena[nameOff], int(nameLen))
						mask := computeCharMask(nameStr)
						packedParent := makeParentField(parentFrn, drive[0], isDir, false)

						if idx, exists := vol.ByFRN[frn]; exists {
							if int(idx) < len(e.entries) {
								e.entries[idx].ParentFRN = packedParent
								e.entries[idx].NameOff = nameOff
								e.entries[idx].NameLen = nameLen
								e.entries[idx].CharMask = mask
								e.entries[idx].SetDeleted(false)
							}
						} else {
							newIdx := int32(len(e.entries))
							e.entries = append(e.entries, Entry{
								ParentFRN: packedParent,
								CharMask:  mask,
								NameOff:   nameOff,
								NameLen:   nameLen,
							})
							vol.ByFRN[frn] = newIdx
						}
					}
				}
			}
			offset += recordLen
		}
		e.mu.Unlock()
	}
}

// ReconstructPath reconstruiește calea completă urcând în map[uint64]int32 al volumului aferent
func (e *Engine) ReconstructPath(entry *Entry) (string, bool) {
	if entry == nil || entry.Deleted() {
		return "", false
	}
	vol, ok := e.volumes[entry.Drive()]
	if !ok {
		return "", false
	}

	var parts [64]string
	partCount := 0
	parts[partCount] = e.getName(entry)
	partCount++

	curr := entry.GetParentFRN()
	depth := 0
	for curr != 0 && depth < 63 {
		parentIdx, exists := vol.ByFRN[curr]
		if !exists || int(parentIdx) >= len(e.entries) {
			break
		}
		parent := &e.entries[parentIdx]
		if parent.Deleted() {
			break
		}
		pName := e.getName(parent)
		if isIgnoredFolderName(pName) {
			return "", false
		}
		parts[partCount] = pName
		partCount++
		if curr == 5 || parent.GetParentFRN() == curr {
			break
		}
		curr = parent.GetParentFRN()
		depth++
	}

	var sb strings.Builder
	for i := partCount - 1; i >= 0; i-- {
		sb.WriteString(parts[i])
		if i > 0 {
			sb.WriteByte('\\')
		}
	}
	return sb.String(), true
}

// FallbackConcurrentWalk este folosit doar dacă nu există drepturi de Admin / SYSTEM
func (e *Engine) FallbackConcurrentWalk(root string) {
	drive := filepath.VolumeName(root)
	if drive == "" {
		drive = "C:"
	}
	driveByte := drive[0]

	byFRN := make(map[uint64]int32)
	var counter uint64 = 10

	e.mu.Lock()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		name := d.Name()
		if d.IsDir() && (isIgnoredFolderName(name) || strings.HasPrefix(name, ".")) {
			return filepath.SkipDir
		}

		parentDir := filepath.Dir(path)
		counter++
		dirID := counter
		dirIdx := int32(len(e.entries))
		pOff, pLen := e.appendName(parentDir)
		e.entries = append(e.entries, Entry{
			ParentFRN: makeParentField(0, driveByte, true, false),
			CharMask:  computeCharMask(parentDir),
			NameOff:   pOff,
			NameLen:   pLen,
		})
		byFRN[dirID] = dirIdx

		counter++
		fileID := counter
		fileIdx := int32(len(e.entries))
		nOff, nLen := e.appendName(name)
		e.entries = append(e.entries, Entry{
			ParentFRN: makeParentField(dirID, driveByte, d.IsDir(), false),
			CharMask:  computeCharMask(name),
			NameOff:   nOff,
			NameLen:   nLen,
		})
		byFRN[fileID] = fileIdx
		return nil
	})

	e.volumes[drive] = &VolumeIndex{Drive: drive, ByFRN: byFRN}
	e.mu.Unlock()
}

// Search executes unified Spotlight search (Applications, Actions, Calculations, Folders, Files, and Web)
func (e *Engine) Search(rawQuery string, limit int) []FileItem {
	if limit <= 0 {
		limit = 8
	}
	if limit > 80 {
		limit = 80
	}

	q := strings.TrimSpace(rawQuery)
	if q == "" {
		return []FileItem{}
	}

	var results []FileItem
	seenPaths := make(map[string]bool, limit)
	qLower := strings.ToLower(q)

	// 1. Calcule matematice instantanee (ex: 2+2, 50*12, =100/4)
	if val, ok := evalMathExpression(strings.TrimPrefix(q, "=")); ok {
		calcItem := FileItem{
			Name:      fmt.Sprintf("= %v", val),
			Path:      fmt.Sprintf("cmd:copy:%v", val),
			Extension: "CALC",
			Category:  "Top Hit",
			Score:     400,
		}
		results = append(results, calcItem)
		seenPaths[calcItem.Path] = true
	}

	// 2. Quick system actions and commands
	type sysAction struct {
		Triggers []string
		Name     string
		Path     string
		Category string
	}
	sysActions := []sysAction{
		{Triggers: []string{"lock", "blocheaza", "blocare"}, Name: "Lock Screen (Lock Workstation)", Path: "cmd:lock", Category: "Actions"},
		{Triggers: []string{"sleep", "somn", "repaus"}, Name: "Put PC to Sleep", Path: "cmd:sleep", Category: "Actions"},
		{Triggers: []string{"restart", "reporneste", "reboot"}, Name: "Restart Computer", Path: "cmd:restart", Category: "Actions"},
		{Triggers: []string{"shutdown", "inchide", "oprire", "power off"}, Name: "Shut Down Computer", Path: "cmd:shutdown", Category: "Actions"},
		{Triggers: []string{"recycle", "emptybin", "empty", "bin", "trash"}, Name: "Empty Recycle Bin", Path: "cmd:emptybin", Category: "Actions"},
		{Triggers: []string{"ip", "ipconfig", "network", "my ip"}, Name: "Local IP: " + getLocalIP() + " (Press Enter to Copy)", Path: "cmd:copy:" + getLocalIP(), Category: "Actions"},
		{Triggers: []string{"settings", "setari", ":settings", ":setari"}, Name: "Open Spotlight Settings and Shortcuts", Path: "cmd:settings", Category: "Actions"},
	}

	for _, sa := range sysActions {
		matched := false
		for _, tr := range sa.Triggers {
			if tr == qLower || strings.HasPrefix(tr, qLower) || (len(qLower) >= 3 && strings.Contains(tr, qLower)) {
				matched = true
				break
			}
		}
		if matched && !seenPaths[sa.Path] {
			cat := sa.Category
			if len(results) == 0 {
				cat = "Top Hit"
			}
			seenPaths[sa.Path] = true
			results = append(results, FileItem{
				Name:      sa.Name,
				Path:      sa.Path,
				Extension: "SYS",
				Category:  cat,
				Score:     350,
			})
		}
	}

	// 3. Extract search terms and extension filter
	extFilter := ""
	searchTerms := strings.Fields(qLower)
	cleanTerms := make([]string, 0, len(searchTerms))

	for _, term := range searchTerms {
		if strings.HasPrefix(term, "ext:") && len(term) > 4 {
			extFilter = "." + strings.TrimPrefix(term, "ext:")
		} else {
			cleanTerms = append(cleanTerms, term)
		}
	}
	queryClean := strings.Join(cleanTerms, " ")
	if queryClean == "" && extFilter == "" {
		return results
	}

	// 4. Direct URL / Domain opening when a website is typed
	isURL := strings.HasPrefix(qLower, "http://") || strings.HasPrefix(qLower, "https://") || strings.HasPrefix(qLower, "www.")
	hasDomain := !strings.Contains(qLower, " ") && (strings.HasSuffix(qLower, ".com") || strings.HasSuffix(qLower, ".org") || strings.HasSuffix(qLower, ".net") || strings.HasSuffix(qLower, ".ro") || strings.HasSuffix(qLower, ".io") || strings.HasSuffix(qLower, ".dev") || strings.HasSuffix(qLower, ".app") || strings.HasSuffix(qLower, ".ai") || strings.HasSuffix(qLower, ".co") || strings.HasSuffix(qLower, ".edu"))
	if isURL || hasDomain {
		destURL := q
		if !strings.HasPrefix(destURL, "http://") && !strings.HasPrefix(destURL, "https://") {
			destURL = "https://" + destURL
		}
		if !seenPaths[destURL] {
			seenPaths[destURL] = true
			results = append(results, FileItem{
				Name:      "Open Website: " + q,
				Path:      destURL,
				Extension: "WEB",
				Category:  "Actions",
				Score:     260,
			})
		}
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	// 5. Search in Application Catalog (Top Hit & Suggested Applications)
	if extFilter == "" && queryClean != "" {
		type appMatch struct {
			app   CatalogApp
			score int
		}
		var appMatches []appMatch
		for _, app := range e.apps {
			if sc, ok := matchApp(app.Name, queryClean); ok {
				appMatches = append(appMatches, appMatch{app: app, score: sc})
			}
		}
		sort.Slice(appMatches, func(i, j int) bool {
			return appMatches[i].score > appMatches[j].score
		})

		for i := 0; i < len(appMatches) && i < 6 && len(results) < limit; i++ {
			p := appMatches[i].app.Path
			if seenPaths[strings.ToLower(p)] {
				continue
			}
			seenPaths[strings.ToLower(p)] = true
			cat := "Applications"
			if len(results) == 0 && i == 0 {
				cat = "Top Hit"
			}
			results = append(results, FileItem{
				Name:      appMatches[i].app.Name,
				Path:      p,
				Extension: "APP",
				Category:  cat,
				Score:     appMatches[i].score,
			})
		}
	}

	// 6. Parallel MFT search for files and folders
	total := len(e.entries)
	if total > 0 && len(results) < limit {
		queryMask := computeCharMask(queryClean)
		queryUniqueBits := bits.OnesCount64(queryMask)

		numWorkers := runtime.NumCPU()
		chunkSize := (total + numWorkers - 1) / numWorkers
		workerMatches := make([][]ScoredEntry, numWorkers)

		var wg sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			startIdx := w * chunkSize
			endIdx := startIdx + chunkSize
			if endIdx > total {
				endIdx = total
			}
			if startIdx >= endIdx {
				break
			}

			wg.Add(1)
			go func(workerID, start, end int) {
				defer wg.Done()
				local := make([]ScoredEntry, 0, 64)

				for i := start; i < end; i++ {
					entry := &e.entries[i]
					if entry.Deleted() || entry.NameLen == 0 {
						continue
					}

					entryName := e.getName(entry)
					if extFilter != "" && !hasSuffixFold(entryName, extFilter) {
						continue
					}

					score := 10
					if queryClean != "" {
						score = scoreAdvanced(entryName, entry.CharMask, queryClean, queryMask, queryUniqueBits)
					}

					if score > 0 {
						if entry.IsDir() {
							score += 20
						} else if hasSuffixFold(entryName, ".lnk") || hasSuffixFold(entryName, ".exe") {
							score += 15
						}
						local = append(local, ScoredEntry{Index: i, Score: score})
					}
				}
				workerMatches[workerID] = local
			}(w, startIdx, endIdx)
		}
		wg.Wait()

		var allMatches []ScoredEntry
		for _, m := range workerMatches {
			allMatches = append(allMatches, m...)
		}

		sort.Slice(allMatches, func(i, j int) bool {
			if allMatches[i].Score == allMatches[j].Score {
				return e.entries[allMatches[i].Index].NameLen < e.entries[allMatches[j].Index].NameLen
			}
			return allMatches[i].Score > allMatches[j].Score
		})

		for _, m := range allMatches {
			if len(results) >= limit {
				break
			}
			entry := &e.entries[m.Index]
			fullPath, valid := e.ReconstructPath(entry)
			if !valid {
				continue
			}
			lowerPath := strings.ToLower(fullPath)
			if seenPaths[lowerPath] {
				continue
			}

			// Smart filtering: exclude internal library, cache, and build files
			if strings.Contains(lowerPath, `\node_modules\`) ||
				strings.Contains(lowerPath, `\site-packages\`) ||
				strings.Contains(lowerPath, `\appdata\local\temp\`) ||
				strings.Contains(lowerPath, `\appdata\local\packages\`) ||
				strings.Contains(lowerPath, `\.git\`) ||
				strings.Contains(lowerPath, `\__pycache__\`) ||
				strings.Contains(lowerPath, `\winsxs\`) ||
				strings.Contains(lowerPath, `\driverstore\`) ||
				strings.Contains(lowerPath, `\obj\`) ||
				strings.Contains(lowerPath, `\bin\debug\`) {
				continue
			}

			score := m.Score
			// Boost for common user locations (Desktop, Documents, Downloads)
			if strings.Contains(lowerPath, `\desktop`) ||
				strings.Contains(lowerPath, `\documents`) ||
				strings.Contains(lowerPath, `\downloads`) {
				score += 30
			}

			seenPaths[lowerPath] = true
			entryName := e.getName(entry)
			ext := filepath.Ext(entryName)
			cat := "Files"
			if entry.IsDir() {
				ext = "DIR"
				cat = "Folders"
			}

			results = append(results, FileItem{
				Name:      entryName,
				Path:      fullPath,
				Extension: ext,
				Category:  cat,
				Score:     score,
			})
		}
	}

	// 7. Fallback Web search like iOS / Spotlight (always available)
	if len(results) < limit && queryClean != "" {
		googleURL := fmt.Sprintf("https://www.google.com/search?q=%s", url.QueryEscape(queryClean))
		if !seenPaths[googleURL] {
			seenPaths[googleURL] = true
			results = append(results, FileItem{
				Name:      fmt.Sprintf("Search Google: %s", queryClean),
				Path:      googleURL,
				Extension: "WEB",
				Category:  "Web Search",
				Score:     10,
			})
		}
	}

	return results
}

func evalMathExpression(exprStr string) (float64, bool) {
	exprStr = strings.ReplaceAll(strings.TrimSpace(exprStr), ",", ".")
	if !strings.ContainsAny(exprStr, "+-*/^()") {
		return 0, false
	}
	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		return 0, false
	}
	val, err := evalAST(expr)
	if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, false
	}
	return math.Round(val*10000) / 10000, true
}

func evalAST(expr ast.Expr) (float64, error) {
	switch n := expr.(type) {
	case *ast.BasicLit:
		if n.Kind == token.INT || n.Kind == token.FLOAT {
			return strconv.ParseFloat(n.Value, 64)
		}
	case *ast.ParenExpr:
		return evalAST(n.X)
	case *ast.UnaryExpr:
		val, err := evalAST(n.X)
		if err != nil {
			return 0, err
		}
		if n.Op == token.SUB {
			return -val, nil
		}
		return val, nil
	case *ast.BinaryExpr:
		left, err := evalAST(n.X)
		if err != nil {
			return 0, err
		}
		right, err := evalAST(n.Y)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, fmt.Errorf("div by 0")
			}
			return left / right, nil
		case token.XOR:
			return math.Pow(left, right), nil
		}
	}
	return 0, fmt.Errorf("invalid")
}

func handleSystemCommands(cmd string) []FileItem {
	commands := []struct {
		Trigger string
		Title   string
		Action  string
	}{
		{":settings", "Open Spotlight Settings (:settings)", "cmd:settings"},
		{":setari", "Open Spotlight Settings (:settings)", "cmd:settings"},
		{"lock", "Lock Windows Workstation", "cmd:lock"},
		{"sleep", "Put PC to Sleep", "cmd:sleep"},
		{"ip", "Local IP: " + getLocalIP() + " (Press Enter to Copy)", "cmd:copy:" + getLocalIP()},
		{"cmd", "Open Command Prompt", `C:\Windows\System32\cmd.exe`},
		{"wt", "Open Windows Terminal", "wt.exe"},
	}

	var out []FileItem
	for _, c := range commands {
		if cmd == "" || strings.Contains(c.Trigger, cmd) || strings.Contains(strings.ToLower(c.Title), cmd) {
			out = append(out, FileItem{
				Name:      c.Title,
				Path:      c.Action,
				Extension: "SYS",
				Category:  "Top Hit",
			})
		}
	}
	return out
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}

func executeSpecialCommand(action string) {
	if strings.HasPrefix(action, "cmd:copy:") {
		text := strings.TrimPrefix(action, "cmd:copy:")
		cmd := exec.Command("cmd", "/c", "echo|set /p="+text+"|clip")
		_ = cmd.Run()
	} else if action == "cmd:lock" {
		_ = exec.Command("rundll32.exe", "user32.dll,LockWorkStation").Start()
	} else if action == "cmd:sleep" {
		_ = exec.Command("rundll32.exe", "powrprof.dll,SetSuspendState", "0,1,0").Start()
	} else if action == "cmd:restart" {
		_ = exec.Command("shutdown", "/r", "/t", "0").Start()
	} else if action == "cmd:shutdown" {
		_ = exec.Command("shutdown", "/s", "/t", "0").Start()
	} else if action == "cmd:emptybin" {
		_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "Clear-RecycleBin -Force -ErrorAction SilentlyContinue").Start()
	}
}

func StartTCPServer(engine *Engine, addr string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleTCPClient(conn, engine)
	}
}

func handleTCPClient(conn net.Conn, engine *Engine) {
	defer conn.Close()
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		start := time.Now()
		query := line
		limit := 24

		if strings.HasPrefix(line, "{") {
			var req struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			if json.Unmarshal([]byte(line), &req) == nil {
				query = req.Q
				if req.Limit > 0 {
					limit = req.Limit
				}
			}
		} else if idx := strings.IndexByte(line, '|'); idx > 0 {
			if l, err := strconv.Atoi(line[:idx]); err == nil && l > 0 {
				limit = l
				query = line[idx+1:]
			}
		}

		if strings.HasPrefix(query, "cmd:") {
			executeSpecialCommand(query)
			_, _ = conn.Write([]byte("{\"status\":\"ok\"}\n"))
			continue
		}

		results := engine.Search(query, limit)
		elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0

		resp := TCPResponse{
			Query:     query,
			LatencyMs: elapsedMs,
			Count:     len(results),
			Results:   results,
		}
		payload, _ := json.Marshal(resp)
		payload = append(payload, '\n')
		_, _ = conn.Write(payload)
	}
}

type serviceStatus struct {
	dwServiceType             uint32
	dwCurrentState            uint32
	dwControlsAccepted        uint32
	dwWin32ExitCode           uint32
	dwServiceSpecificExitCode uint32
	dwCheckPoint              uint32
	dwWaitHint                uint32
}

type serviceTableEntry struct {
	lpServiceName *uint16
	lpServiceProc uintptr
}

var (
	svcStatusHandle uintptr
	svcName         = "SpotlightGoService"
)

func serviceCtrlHandler(control uint32) uintptr {
	if control == SERVICE_CONTROL_STOP {
		setServiceState(SERVICE_STOPPED)
		os.Exit(0)
	}
	return 0
}

func setServiceState(state uint32) {
	if svcStatusHandle == 0 {
		return
	}
	status := serviceStatus{
		dwServiceType:      SERVICE_WIN32_OWN_PROCESS,
		dwCurrentState:     state,
		dwControlsAccepted: SERVICE_ACCEPT_STOP,
	}
	procSetServiceStatus.Call(svcStatusHandle, uintptr(unsafe.Pointer(&status)))
}

func serviceMain(argc uint32, argv **uint16) uintptr {
	namePtr, _ := syscall.UTF16PtrFromString(svcName)
	svcStatusHandle, _, _ = procRegisterServiceCtrlHandler.Call(
		uintptr(unsafe.Pointer(namePtr)),
		syscall.NewCallback(serviceCtrlHandler),
	)
	setServiceState(SERVICE_START_PENDING)
	go runServers()
	setServiceState(SERVICE_RUNNING)
	select {}
}

func tryRunAsWindowsService() {
	namePtr, _ := syscall.UTF16PtrFromString(svcName)
	table := [2]serviceTableEntry{
		{lpServiceName: namePtr, lpServiceProc: syscall.NewCallback(serviceMain)},
		{lpServiceName: nil, lpServiceProc: 0},
	}
	procStartServiceCtrlDispatcher.Call(uintptr(unsafe.Pointer(&table[0])))
}

func handleServiceCLI() bool {
	if len(os.Args) < 2 {
		return false
	}
	exePath, _ := os.Executable()
	switch os.Args[1] {
	case "--install-service":
		_ = exec.Command("sc.exe", "create", svcName, "binPath=", fmt.Sprintf("\"%s\" --service", exePath), "start=", "auto", "obj=", "LocalSystem").Run()
		_ = exec.Command("sc.exe", "start", svcName).Run()
		return true
	case "--uninstall-service":
		_ = exec.Command("sc.exe", "stop", svcName).Run()
		_ = exec.Command("sc.exe", "delete", svcName).Run()
		return true
	case "--service":
		tryRunAsWindowsService()
		return true
	}
	return false
}

func runServers() {
	// Setăm un GC mai agresiv pentru a ține memoria RAM cât mai jos
	debug.SetGCPercent(50)

	engine := NewEngine()

	go engine.IndexAllVolumes()
	go StartTCPServer(engine, "127.0.0.1:49152")

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		limit := 8
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil {
				limit = l
			}
		}
		results := engine.Search(q, limit)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(results)
	})

	http.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		act := r.URL.Query().Get("run")
		executeSpecialCommand(act)
		w.WriteHeader(http.StatusOK)
	})

	port := ":9876"
	_ = http.ListenAndServe(port, nil)
}

func hideConsole() {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		procShowWindow.Call(hwnd, 0)
	}
}

func main() {
	hideConsole()
	if handleServiceCLI() {
		return
	}
	runServers()
}
