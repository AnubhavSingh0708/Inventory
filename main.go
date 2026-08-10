// Inventory Manager — single-file, cross-platform (Windows/Linux) desktop app.
//
// Stack: Fyne v2 (desktop UI + system tray) + Fiber v2 (HTTP API server) + SQLite
// (via mattn/go-sqlite3 — swap for modernc.org/sqlite in go.mod if you prefer a
// pure-Go/no-CGO driver; both use database/sql the same way).

// DATA FILES created next to the binary at runtime:
//
//	inventory.db   - sqlite database (settings, users, cells, logs)
//	logs.txt       - rolling text log, capped at 1024 lines
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/caddyserver/certmagic"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Constants & globals
// ---------------------------------------------------------------------------

const (
	dbFile      = "inventory.db"
	logFile     = "logs.txt"
	maxLogLines = 4096
	assetsDir   = "assets"
)

var (
	fyneApp    fyne.App
	mainWindow fyne.Window
	db         *sql.DB

	fiberApp     *fiber.App
	serverMu     sync.Mutex
	serverUp     bool
	serverPort   string
	serverAddr   string // last known bind address for display
	serverScheme string // "https" (default) or "http" (localhost-only mode)
)

// palette used for cell colour picking in the grid editor
var colorPalette = []string{
	"#4A90D9", "#E74C3C", "#2ECC71", "#F1C40F", "#9B59B6",
	"#1ABC9C", "#E67E22", "#34495E", "#95A5A6", "#D35400",
}

// ---------------------------------------------------------------------------
// Data models
// ---------------------------------------------------------------------------

type User struct {
	ID           int
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    string
}

// AuthKey is one issued device/session credential: a user logs in with their
// username + password once and gets back a key, which every subsequent API
// call presents instead of the password (id + key, not id + password). Each
// login issues a brand new key, so a single user can hold one per device.
type AuthKey struct {
	Key       string `json:"key"`
	UserID    int    `json:"user_id"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used"`
}

type CellItem struct {
	Number    int    `json:"number"`
	MonthCode string `json:"month_code"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
}

// Party is a selectable party name with an optional free-text note attached.
type Party struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"` // may be empty
}

// Table is one physical storage table/rack — a region defined by a filled
// polygon in map.svg (the table's ID matches that polygon's "id" attribute
// exactly). Every cell belongs to exactly one Table. StackType controls how
// items stack inside each of that table's cells:

type Table struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	GridRows  int    `json:"grid_rows"`
	GridCols  int    `json:"grid_cols"`
	StackType string `json:"stack_type"`
	StackCols int    `json:"stack_cols"`
	StackRows int    `json:"stack_rows"`
}

// Cell now belongs to exactly one Table (TableID). (row, col) is only
// unique within a table, not globally — two different tables can each have
// a cell at (0, 0).
type Cell struct {
	TableID string     `json:"table_id"`
	Row     int        `json:"row"`
	Col     int        `json:"col"`
	Color   string     `json:"color"`
	Number  int        `json:"number"`
	Items   []CellItem `json:"items"`
}

// MonthCode is a globally available month code that a CellItem's MonthCode
// can be chosen from (managed centrally, not per-cell).
type MonthCode struct {
	ID   int    `json:"id"`
	Code string `json:"code"`
}

// ChangeEvent describes a single mutation broadcast to /api/events subscribers
// so clients can replicate table changes without re-polling the whole table.
type ChangeEvent struct {
	EventTime string      `json:"event_time"`
	Type      string      `json:"type"`
	Username  string      `json:"username"`
	Detail    interface{} `json:"detail"`
}

// Reel represents one row of the packing/reel record table.
type Reel struct {
	ID         int     `json:"id"`
	ReelID     string  `json:"reel_id"`
	MonthCode  string  `json:"month_code"`
	ItemNumber int     `json:"item_number"`
	SizeCM     float64 `json:"size_cm"`
	GSM        float64 `json:"gsm"`
	BF         string  `json:"bf"`
	Colour     string  `json:"colour"`
	WeightKg   float64 `json:"weight_kg"`
	Date       string  `json:"date"` // YYYY-MM-DD
	Time       string  `json:"time"` // HH:MM:SS, packing time
	Quality    string  `json:"quality"`
	Party      string  `json:"party"` // chosen from the global parties list
}

// dispatch and billing structs
// ---------------------------------------------------------------------------
// Billing & Archive Models
// ---------------------------------------------------------------------------

type BillingReel struct {
	Reel
	DispatchDate string `json:"dispatch_date"`
	DispatchTime string `json:"dispatch_time"`
}

type BilledArchiveReel struct {
	BillingReel
	BilledDate string `json:"billed_date"`
	BilledTime string `json:"billed_time"`
}

// Request payloads for the new APIs
type dispatchAddReq struct {
	UserID       int    `json:"user_id"`
	AuthKey      string `json:"auth_key"`
	ReelID       string `json:"reel_id"`
	DispatchDate string `json:"dispatch_date"` // YYYY-MM-DD
	DispatchTime string `json:"dispatch_time"` // HH:MM:SS
}

type billedArchiveReq struct {
	UserID     int    `json:"user_id"`
	AuthKey    string `json:"auth_key"`
	ReelID     string `json:"reel_id"`
	BilledDate string `json:"billed_date"` // YYYY-MM-DD
	BilledTime string `json:"billed_time"` // HH:MM:SS
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	serverScheme = "https"

	var err error
	db, err = openDB(dbFile)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	if err := migrateDB(db); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	fyneApp = app.NewWithID("com.inventorymanager.app")
	fyneApp.Settings().SetTheme(theme.DefaultTheme())
	mainWindow = fyneApp.NewWindow("Inventory Manager")
	mainWindow.Resize(fyne.NewSize(980, 680))
	mainWindow.SetIcon(loadResource("app_icon.png", theme.StorageIcon()))

	// Closing the window should hide it, not kill the process, so the server
	// (and tray icon) keep running in the background.
	mainWindow.SetCloseIntercept(func() {
		mainWindow.Hide()
	})

	setupTray()

	if getSetting("setup_complete") == "true" {
		showDashboard()
		// If a port was previously configured, auto-start the server.
		if p := getSetting("server_port"); p != "" {
			go startServer(p)
		}
	} else {
		showSetupWizard()
	}

	mainWindow.ShowAndRun()
}

// setupTray wires up a system tray icon + menu so the app keeps a visible
// status even when the main window is hidden. Only available on desktop
// platforms; safe no-op elsewhere.
func setupTray() {
	deskApp, ok := fyneApp.(desktop.App)
	if !ok {
		return
	}
	menu := fyne.NewMenu("Inventory Manager",
		fyne.NewMenuItem("Show Window", func() {
			mainWindow.Show()
			mainWindow.RequestFocus()
		}),
		fyne.NewMenuItem("Server Status", func() {
			status := "stopped"
			if isServerRunning() {
				status = "running"
			}
			dialog.ShowInformation("Server Status", "The server is currently: "+status, mainWindow)
		}),
		fyne.NewMenuItem("Quit", func() {
			stopServer()
			fyneApp.Quit()
		}),
	)
	deskApp.SetSystemTrayMenu(menu)
	updateTrayIcon()
}

func updateTrayIcon() {
	deskApp, ok := fyneApp.(desktop.App)
	if !ok {
		return
	}
	if isServerRunning() {
		deskApp.SetSystemTrayIcon(loadResource("tray_running.png", theme.ConfirmIcon()))
	} else {
		deskApp.SetSystemTrayIcon(loadResource("tray_stopped.png", theme.ErrorIcon()))
	}
}

func loadResource(name string, fallback fyne.Resource) fyne.Resource {
	path := assetsDir + string(os.PathSeparator) + name
	if _, err := os.Stat(path); err != nil {
		return fallback
	}
	res, err := fyne.LoadResourceFromPath(path)
	if err != nil {
		return fallback
	}
	return res
}

// ---------------------------------------------------------------------------
// Database setup
// ---------------------------------------------------------------------------

func openDB(path string) (*sql.DB, error) {
	// Add _synchronous=NORMAL for WAL mode performance boost
	conn, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}

	// WAL mode allows up to 25 concurrent readers without blocking writes
	conn.SetMaxOpenConns(25)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)

	// SQLite tuning pragmas for lower latency
	_, _ = conn.Exec(`
		PRAGMA cache_size = -200000; -- Allocate 200MB memory cache for reads
		PRAGMA temp_store = MEMORY; -- Store temporary tables in RAM
	`)
	return conn, nil
}

func migrateDB(conn *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS auth_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT UNIQUE NOT NULL,
			user_id INTEGER NOT NULL,
			username TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT '',
			last_used TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS tables (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			grid_rows INTEGER NOT NULL DEFAULT 1,
			grid_cols INTEGER NOT NULL DEFAULT 1,
			stack_type TEXT NOT NULL DEFAULT 'vertical',
			stack_cols INTEGER NOT NULL DEFAULT 1,
			stack_rows INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS cells (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			table_id TEXT NOT NULL REFERENCES tables(id) ON DELETE CASCADE,
			row INTEGER NOT NULL,
			col INTEGER NOT NULL,
			color TEXT NOT NULL DEFAULT '#4A90D9',
			number INTEGER NOT NULL DEFAULT 0,
			items TEXT NOT NULL DEFAULT '[]',
			UNIQUE(table_id, row, col)
		)`,
		`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS reels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reel_id TEXT NOT NULL DEFAULT '',
			month_code TEXT NOT NULL DEFAULT '',
			item_number INTEGER NOT NULL DEFAULT 0,
			size_cm REAL NOT NULL DEFAULT 0,
			gsm REAL NOT NULL DEFAULT 0,
			bf TEXT NOT NULL DEFAULT '',
			colour TEXT NOT NULL DEFAULT '',
			weight_kg REAL NOT NULL DEFAULT 0,
			date TEXT NOT NULL DEFAULT '',
			time TEXT NOT NULL DEFAULT '',
			quality TEXT NOT NULL DEFAULT '',
			party TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS parties (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS month_codes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			code TEXT NOT NULL UNIQUE
		)`,
		`CREATE TABLE IF NOT EXISTS billing (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reel_id TEXT NOT NULL UNIQUE,
			month_code TEXT NOT NULL DEFAULT '',
			item_number INTEGER NOT NULL DEFAULT 0,
			size_cm REAL NOT NULL DEFAULT 0,
			gsm REAL NOT NULL DEFAULT 0,
			bf TEXT NOT NULL DEFAULT '',
			colour TEXT NOT NULL DEFAULT '',
			weight_kg REAL NOT NULL DEFAULT 0,
			date TEXT NOT NULL DEFAULT '',
			time TEXT NOT NULL DEFAULT '',
			quality TEXT NOT NULL DEFAULT '',
			party TEXT NOT NULL DEFAULT '',
			dispatch_date TEXT NOT NULL DEFAULT '',
			dispatch_time TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS billed_archive (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reel_id TEXT NOT NULL UNIQUE,
			month_code TEXT NOT NULL DEFAULT '',
			item_number INTEGER NOT NULL DEFAULT 0,
			size_cm REAL NOT NULL DEFAULT 0,
			gsm REAL NOT NULL DEFAULT 0,
			bf TEXT NOT NULL DEFAULT '',
			colour TEXT NOT NULL DEFAULT '',
			weight_kg REAL NOT NULL DEFAULT 0,
			date TEXT NOT NULL DEFAULT '',
			time TEXT NOT NULL DEFAULT '',
			quality TEXT NOT NULL DEFAULT '',
			party TEXT NOT NULL DEFAULT '',
			dispatch_date TEXT NOT NULL DEFAULT '',
			dispatch_time TEXT NOT NULL DEFAULT '',
			billed_date TEXT NOT NULL DEFAULT '',
			billed_time TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reels_reel_id ON reels(reel_id)`,
		`CREATE INDEX IF NOT EXISTS idx_reels_item_month ON reels(item_number, month_code)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_keys_lookup ON auth_keys(user_id, key)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	_, _ = conn.Exec(`ALTER TABLE reels ADD COLUMN party TEXT NOT NULL DEFAULT ''`)
	return nil
}

// ---------------------------------------------------------------------------
// Settings key/value helpers
// ---------------------------------------------------------------------------

func getSetting(key string) string {
	var val string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err != nil {
		return ""
	}
	return val
}

func setSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------------------
// User helpers
// ---------------------------------------------------------------------------

func createUser(username, plainPassword string, isAdmin bool) (int64, error) {
	hash, err := hashPassword(plainPassword)
	if err != nil {
		return 0, err
	}
	admin := 0
	if isAdmin {
		admin = 1
	}
	res, err := db.Exec(`INSERT INTO users (username, password_hash, is_admin, created_at)
		VALUES (?, ?, ?, datetime('now'))`, username, hash, admin)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func getUserByID(id int) (*User, error) {
	u := &User{}
	var admin int
	err := db.QueryRow(`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin == 1
	return u, nil
}

func getUserByUsername(username string) (*User, error) {
	u := &User{}
	var admin int
	err := db.QueryRow(`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin == 1
	return u, nil
}

func listUsers() ([]User, error) {
	rows, err := db.Query(`SELECT id, username, password_hash, is_admin, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var admin int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = admin == 1
		out = append(out, u)
	}
	return out, nil
}

func deleteUser(id int) error {
	_, err := db.Exec(`DELETE FROM users WHERE id = ? AND is_admin = 0`, id)
	return err
}

func updateUserPassword(id int, newPlainPassword string) error {
	hash, err := hashPassword(newPlainPassword)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// ---------------------------------------------------------------------------
// Auth key helpers
//
// Passwords are only ever used once, at /api/login. That call hands back an
// auth key, and every other endpoint is authenticated with user_id + that
// key instead of the password itself — so the password never has to be
// resent (and re-risked) on every single API call.
//
// Bookkeeping keeps the auth_keys table from growing forever:
//   - each login adds a new key stamped with the current time
//   - per user, only the most recent authKeysPerUserLimit keys are kept —
//     logging in from a new device pushes the oldest one out
//   - any key (any user) that hasn't been used in authKeyIdleDays days is
//     dropped outright, logged-in device or not
// ---------------------------------------------------------------------------

const (
	authKeyBytes         = 32
	authKeysPerUserLimit = 10
	authKeyIdleDays      = 7
)

// newAuthKeyString returns a fresh cryptographically random hex string
// suitable for use as an auth key.
func newAuthKeyString() (string, error) {
	b := make([]byte, authKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// createAuthKey issues a brand new auth key for a just-logged-in user,
// records it (key, user id, username, timestamps), then runs housekeeping
func createAuthKey(userID int, username string) (string, error) {
	key, err := newAuthKeyString()
	if err != nil {
		return "", fmt.Errorf("failed to generate auth key: %w", err)
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`INSERT INTO auth_keys (key, user_id, username, created_at, last_used)
		VALUES (?, ?, ?, ?, ?)`, key, userID, username, now, now); err != nil {
		return "", err
	}
	trimAuthKeysForUser(userID)
	cleanupStaleAuthKeys()
	return key, nil
}

// trimAuthKeysForUser keeps only the authKeysPerUserLimit most recently
// issued keys for a user, deleting any older ones.
func trimAuthKeysForUser(userID int) {
	_, _ = db.Exec(`DELETE FROM auth_keys WHERE user_id = ? AND id NOT IN (
		SELECT id FROM auth_keys WHERE user_id = ? ORDER BY id DESC LIMIT ?)`,
		userID, userID, authKeysPerUserLimit)
}

// cleanupStaleAuthKeys deletes any auth key (across all users) that hasn't
// been used in authKeyIdleDays days.
func cleanupStaleAuthKeys() {
	cutoff := time.Now().AddDate(0, 0, -authKeyIdleDays).Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`DELETE FROM auth_keys WHERE last_used < ?`, cutoff)
}

// ---------------------------------------------------------------------------
// Table (physical region) helpers
//
// Each Table corresponds 1:1 with a filled shape in map.svg — see the Table
// doc comment above for what its fields mean and for the vertical/horizontal
// stacking assumption.
// ---------------------------------------------------------------------------

// defaultTable returns a Table with sane placeholder parameters for a
// freshly discovered map region that hasn't been configured yet.
func defaultTable(id string) Table {
	return Table{ID: id, Name: id, GridRows: 1, GridCols: 1, StackType: "vertical", StackCols: 3, StackRows: 3}
}

func validateTableParams(t Table) error {
	if t.ID == "" {
		return fmt.Errorf("table id is required")
	}
	if t.StackType != "vertical" && t.StackType != "horizontal" {
		return fmt.Errorf(`stack_type must be "vertical" or "horizontal"`)
	}
	if t.GridRows < 1 || t.GridCols < 1 {
		return fmt.Errorf("grid_rows and grid_cols must each be at least 1")
	}
	if t.StackCols < 1 || t.StackRows < 1 {
		return fmt.Errorf("stack_cols and stack_rows must each be at least 1")
	}
	if t.StackType == "horizontal" && t.StackRows > t.StackCols {
		return fmt.Errorf("stack_rows (%d) cannot exceed stack_cols (%d) for horizontal (tapering) stacking",
			t.StackRows, t.StackCols)
	}
	return nil
}

func createTable(t Table) error {
	if err := validateTableParams(t); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO tables (id, name, grid_rows, grid_cols, stack_type, stack_cols, stack_rows)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.GridRows, t.GridCols, t.StackType, t.StackCols, t.StackRows)
	return err
}

func updateTable(t Table) error {
	if err := validateTableParams(t); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE tables SET name = ?, grid_rows = ?, grid_cols = ?, stack_type = ?, stack_cols = ?, stack_rows = ?
		WHERE id = ?`,
		t.Name, t.GridRows, t.GridCols, t.StackType, t.StackCols, t.StackRows, t.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no table found with id %q", t.ID)
	}
	return nil
}

// deleteTable removes a table and, via ON DELETE CASCADE, every cell that
// belonged to it.
func deleteTable(id string) error {
	res, err := db.Exec(`DELETE FROM tables WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no table found with id %q", id)
	}
	return nil
}

func getTable(id string) (*Table, error) {
	var t Table
	err := db.QueryRow(`SELECT id, name, grid_rows, grid_cols, stack_type, stack_cols, stack_rows FROM tables WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.GridRows, &t.GridCols, &t.StackType, &t.StackCols, &t.StackRows)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func getAllTables() ([]Table, error) {
	rows, err := db.Query(`SELECT id, name, grid_rows, grid_cols, stack_type, stack_cols, stack_rows FROM tables ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Table, 0)
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.ID, &t.Name, &t.GridRows, &t.GridCols, &t.StackType, &t.StackCols, &t.StackRows); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Map (map.svg) helpers
//
// The floor-plan SVG uploaded during setup contains one filled shape per
// physical table — that shape's "id" attribute is used directly as the
// table's ID (see the Table doc comment). It also contains exactly two
// small reference-point objects, id="refpoint1" and id="refpoint2", which
// the frontend uses to calibrate/align the drawing; the backend has no use
// for their coordinates and only ever excludes their ids from the
// discovered table list.
// ---------------------------------------------------------------------------

const mapSVGFile = "map.svg"

// mapShapeTags lists the SVG element types treated as "a filled region" —
// i.e. a candidate table — when scanning the map for ids.
var mapShapeTags = map[string]bool{
	"polygon": true, "path": true, "rect": true,
	"circle": true, "ellipse": true, "polyline": true,
}

// parseMapTableIDs scans an SVG file and returns the id of every shape
// element it contains, excluding reference points (any id beginning with
// "refpoint", case-insensitive). Order matches the order shapes appear in
// the file; duplicate ids are only returned once.
func parseMapTableIDs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	var ids []string
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid SVG: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !mapShapeTags[strings.ToLower(se.Name.Local)] {
			continue
		}
		var id string
		for _, a := range se.Attr {
			if strings.EqualFold(a.Name.Local, "id") {
				id = a.Value
				break
			}
		}
		if id == "" || strings.HasPrefix(strings.ToLower(id), "refpoint") || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// copyMapFile copies the file at srcPath to ./map.svg (next to the running
// binary), overwriting any existing map. Used both by the setup wizard and
// by the dashboard's "Change Map" action.
func copyMapFile(srcPath string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return os.WriteFile(mapSVGFile, data, 0644)
}

// mapFileExists reports whether ./map.svg has been uploaded yet.
func mapFileExists() bool {
	_, err := os.Stat(mapSVGFile)
	return err == nil
}

// ---------------------------------------------------------------------------
// Cell / stack helpers — every cell belongs to exactly one Table, and the
// item-stack shape (validateStackPosition) is driven by that table's own
// StackType/StackCols/StackRows instead of a single global setting.
// ---------------------------------------------------------------------------

// validateStackPosition checks that (x, y) is a legal slot in table t's
// configured stack. See the Table doc comment for what "vertical" vs
// "horizontal" mean.
func validateStackPosition(t *Table, x, y int) error {
	if y < 0 || y >= t.StackRows {
		return fmt.Errorf("y must be between 0 and %d (table %q has %d stack level(s))", t.StackRows-1, t.ID, t.StackRows)
	}
	levelSlots := t.StackCols
	if t.StackType == "horizontal" {
		levelSlots = t.StackCols - y
	}
	if x < 0 || x >= levelSlots {
		return fmt.Errorf("x must be between 0 and %d at level y=%d (that level has %d slot(s))", levelSlots-1, y, levelSlots)
	}
	return nil
}

// validateCellItems checks every item's stack position is valid for table t
// and that no two items in the list collide on item number or on (x, y).
func validateCellItems(t *Table, items []CellItem) error {
	seenNumber := map[int]bool{}
	seenPos := map[[2]int]bool{}
	for _, it := range items {
		if err := validateStackPosition(t, it.X, it.Y); err != nil {
			return fmt.Errorf("item %d: %w", it.Number, err)
		}
		if seenNumber[it.Number] {
			return fmt.Errorf("item %d appears more than once", it.Number)
		}
		seenNumber[it.Number] = true
		pos := [2]int{it.X, it.Y}
		if seenPos[pos] {
			return fmt.Errorf("position (x=%d, y=%d) is used by more than one item", it.X, it.Y)
		}
		seenPos[pos] = true
	}
	return nil
}

func saveCell(c Cell) error {
	items, err := json.Marshal(c.Items)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO cells (table_id, row, col, color, number, items) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(table_id, row, col) DO UPDATE SET color = excluded.color, number = excluded.number,
			items = excluded.items`,
		c.TableID, c.Row, c.Col, c.Color, c.Number, string(items))
	return err
}

// getAllCells returns every cell belonging to tableID. Pass an empty string
// to get every cell across every table — used by the search API and other
// table-agnostic reporting.
func getAllCells(tableID string) ([]Cell, error) {
	var rows *sql.Rows
	var err error
	if tableID == "" {
		rows, err = db.Query(`SELECT table_id, row, col, color, number, items FROM cells ORDER BY table_id, row, col`)
	} else {
		rows, err = db.Query(`SELECT table_id, row, col, color, number, items FROM cells WHERE table_id = ? ORDER BY row, col`, tableID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Cell, 0)
	for rows.Next() {
		var c Cell
		var itemsRaw string
		if err := rows.Scan(&c.TableID, &c.Row, &c.Col, &c.Color, &c.Number, &itemsRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(itemsRaw), &c.Items)
		if c.Items == nil {
			c.Items = []CellItem{}
		}
		out = append(out, c)
	}
	return out, nil
}

func getCell(tableID string, row, col int) (*Cell, error) {
	var c Cell
	var itemsRaw string
	err := db.QueryRow(`SELECT table_id, row, col, color, number, items FROM cells WHERE table_id = ? AND row = ? AND col = ?`,
		tableID, row, col).
		Scan(&c.TableID, &c.Row, &c.Col, &c.Color, &c.Number, &itemsRaw)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(itemsRaw), &c.Items)
	if c.Items == nil {
		c.Items = []CellItem{}
	}
	return &c, nil
}

func deleteCellsOutsideBounds(tableID string, rows, cols int) error {
	_, err := db.Exec(`DELETE FROM cells WHERE table_id = ? AND (row >= ? OR col >= ?)`, tableID, rows, cols)
	return err
}

// clearAllItems empties every item from every cell in every table — used by
// the admin "Reset Inventory" action. Cell colours/numbers and table
// layout are kept.
func clearAllItems() error {
	_, err := db.Exec(`UPDATE cells SET items = '[]'`)
	return err
}

// replaceEntireTableCells replaces every cell belonging to tableID with the
// given list, leaving every other table's cells untouched.
func replaceEntireTableCells(tableID string, cells []Cell) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cells WHERE table_id = ?`, tableID); err != nil {
		tx.Rollback()
		return err
	}
	for _, c := range cells {
		items, _ := json.Marshal(c.Items)
		if _, err := tx.Exec(`INSERT INTO cells (table_id, row, col, color, number, items) VALUES (?, ?, ?, ?, ?, ?)`,
			tableID, c.Row, c.Col, c.Color, c.Number, string(items)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// setCellItems replaces the entire items list of one cell in table t — used
// by the /api/cellset endpoint to let a client re-send a modified/
// rearranged sequence rather than issuing individual add/remove/move calls.
func setCellItems(t *Table, row, col int, items []CellItem) error {
	if items == nil {
		items = []CellItem{}
	}
	if err := validateCellItems(t, items); err != nil {
		return err
	}
	c, err := getCell(t.ID, row, col)
	if err != nil {
		return err
	}
	c.Items = items
	return saveCell(*c)
}

func removeItemFromCell(tableID string, row, col, itemNumber int) error {
	c, err := getCell(tableID, row, col)
	if err != nil {
		return err
	}
	newItems := make([]CellItem, 0, len(c.Items))
	found := false
	for _, it := range c.Items {
		if it.Number == itemNumber {
			found = true
			continue
		}
		newItems = append(newItems, it)
	}
	if !found {
		return fmt.Errorf("item %d not found in cell (%d,%d) of table %q", itemNumber, row, col, tableID)
	}
	c.Items = newItems
	return saveCell(*c)
}

func addItemToCell(t *Table, row, col int, item CellItem) error {
	if err := validateStackPosition(t, item.X, item.Y); err != nil {
		return err
	}
	c, err := getCell(t.ID, row, col)
	if err != nil {
		return err
	}
	for _, it := range c.Items {
		if it.Number == item.Number {
			return fmt.Errorf("item %d already exists in cell (%d,%d) of table %q", item.Number, row, col, t.ID)
		}
		if it.X == item.X && it.Y == item.Y {
			return fmt.Errorf("position (x=%d, y=%d) is already occupied in cell (%d,%d) of table %q", item.X, item.Y, row, col, t.ID)
		}
	}
	c.Items = append(c.Items, item)
	return saveCell(*c)
}

// moveItem relocates itemNumber from (fromTableID, fromRow, fromCol) into
// (toTable, toRow, toCol) at stack position (toX, toY). The item may move
// within the same table or across two different tables (e.g. re-racking a
// reel from one physical table to another); the destination position is
// always validated against the destination table's own stack shape.
func moveItem(fromTableID string, fromRow, fromCol int, toTable *Table, toRow, toCol, itemNumber, toX, toY int) error {
	if err := validateStackPosition(toTable, toX, toY); err != nil {
		return err
	}

	from, err := getCell(fromTableID, fromRow, fromCol)
	if err != nil {
		return err
	}

	idx := -1
	for i, it := range from.Items {
		if it.Number == itemNumber {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("item %d not found in source cell (%d,%d) of table %q", itemNumber, fromRow, fromCol, fromTableID)
	}
	moved := from.Items[idx]
	from.Items = append(from.Items[:idx], from.Items[idx+1:]...)

	sameCell := fromTableID == toTable.ID && fromRow == toRow && fromCol == toCol
	to := from
	if !sameCell {
		to, err = getCell(toTable.ID, toRow, toCol)
		if err != nil {
			return err
		}
	}

	for _, it := range to.Items {
		if it.X == toX && it.Y == toY {
			return fmt.Errorf("position (x=%d, y=%d) is already occupied in destination cell (%d,%d) of table %q", toX, toY, toRow, toCol, toTable.ID)
		}
	}

	moved.X = toX
	moved.Y = toY
	to.Items = append(to.Items, moved)

	if sameCell {
		return saveCell(*to)
	}
	if err := saveCell(*from); err != nil {
		return err
	}
	return saveCell(*to)
}

// ---------------------------------------------------------------------------
// Month codes — a global picklist (not per-cell). A CellItem's MonthCode is
// chosen from this list, same pattern as Party names.
// ---------------------------------------------------------------------------

func addMonthCode(code string) (int64, error) {
	if code == "" {
		return 0, fmt.Errorf("month code is required")
	}
	res, err := db.Exec(`INSERT INTO month_codes (code) VALUES (?)`, code)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func removeMonthCode(id int) error {
	res, err := db.Exec(`DELETE FROM month_codes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no month code found with id %d", id)
	}
	return nil
}

func getAllMonthCodes() ([]MonthCode, error) {
	rows, err := db.Query(`SELECT id, code FROM month_codes ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonthCode
	for rows.Next() {
		var m MonthCode
		if err := rows.Scan(&m.ID, &m.Code); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Reel records
// ---------------------------------------------------------------------------

// modified addReel function (replaces existing one)
func addReel(r Reel) (int64, error) {
	var count int
	err := db.QueryRow(`
		SELECT COALESCE(SUM(c), 0) FROM (
			SELECT COUNT(*) as c FROM reels WHERE reel_id = ?
			UNION ALL
			SELECT COUNT(*) as c FROM billing WHERE reel_id = ?
			UNION ALL
			SELECT COUNT(*) as c FROM billed_archive WHERE reel_id = ?
		)
	`, r.ReelID, r.ReelID, r.ReelID).Scan(&count)

	if err != nil {
		return 0, fmt.Errorf("failed to check existing reel ID: %w", err)
	}
	if count > 0 {
		return 0, fmt.Errorf("reel ID %q already exists in the system (reels, billing, or archive)", r.ReelID)
	}

	res, err := db.Exec(`INSERT INTO reels (reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ReelID, r.MonthCode, r.ItemNumber, r.SizeCM, r.GSM, r.BF, r.Colour, r.WeightKg, r.Date, r.Time, r.Quality, r.Party)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// moveReelToBilling handles the transaction of moving a record from 'reels' to 'billing'
func moveReelToBilling(reelID, dispatchDate, dispatchTime string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var r Reel
	err = tx.QueryRow(`SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party FROM reels WHERE reel_id = ?`, reelID).
		Scan(&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber, &r.SizeCM, &r.GSM, &r.BF, &r.Colour, &r.WeightKg, &r.Date, &r.Time, &r.Quality, &r.Party)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("reel %q not found in active inventory", reelID)
		}
		return err
	}

	_, err = tx.Exec(`INSERT INTO billing (reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ReelID, r.MonthCode, r.ItemNumber, r.SizeCM, r.GSM, r.BF, r.Colour, r.WeightKg, r.Date, r.Time, r.Quality, r.Party, dispatchDate, dispatchTime)
	if err != nil {
		return fmt.Errorf("failed to insert into billing: %w", err)
	}

	_, err = tx.Exec(`DELETE FROM reels WHERE reel_id = ?`, reelID)
	if err != nil {
		return fmt.Errorf("failed to remove from reels: %w", err)
	}

	return tx.Commit()
}

// moveBillingToArchive handles moving a record from 'billing' to 'billed_archive'
func moveBillingToArchive(reelID, billedDate, billedTime string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var br BillingReel
	err = tx.QueryRow(`SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time FROM billing WHERE reel_id = ?`, reelID).
		Scan(&br.ID, &br.ReelID, &br.MonthCode, &br.ItemNumber, &br.SizeCM, &br.GSM, &br.BF, &br.Colour, &br.WeightKg, &br.Date, &br.Time, &br.Quality, &br.Party, &br.DispatchDate, &br.DispatchTime)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("reel %q not found in billing", reelID)
		}
		return err
	}

	_, err = tx.Exec(`INSERT INTO billed_archive (reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time, billed_date, billed_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		br.ReelID, br.MonthCode, br.ItemNumber, br.SizeCM, br.GSM, br.BF, br.Colour, br.WeightKg, br.Date, br.Time, br.Quality, br.Party, br.DispatchDate, br.DispatchTime, billedDate, billedTime)
	if err != nil {
		return fmt.Errorf("failed to insert into billed_archive: %w", err)
	}

	_, err = tx.Exec(`DELETE FROM billing WHERE reel_id = ?`, reelID)
	if err != nil {
		return fmt.Errorf("failed to remove from billing: %w", err)
	}

	return tx.Commit()
}

// Fetch helper for listing billing
func getAllBilling() ([]BillingReel, error) {
	rows, err := db.Query(`SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time FROM billing ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Initialize as an empty array instead of nil to guarantee valid JSON `[]` when empty
	out := make([]BillingReel, 0)

	for rows.Next() {
		var r BillingReel
		if err := rows.Scan(&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber, &r.SizeCM, &r.GSM, &r.BF, &r.Colour, &r.WeightKg, &r.Date, &r.Time, &r.Quality, &r.Party, &r.DispatchDate, &r.DispatchTime); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Fetch helper for listing archive
func getAllArchive(limit, offset int) ([]BilledArchiveReel, error) {
	if limit <= 0 {
		limit = 100 // default page size
	}
	if limit > 1000 {
		limit = 1000 // cap maximum to prevent RAM abuse
	}

	rows, err := db.Query(`
		SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time, billed_date, billed_time 
		FROM billed_archive 
		ORDER BY id DESC 
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BilledArchiveReel, 0)
	for rows.Next() {
		var r BilledArchiveReel
		if err := rows.Scan(
			&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber, &r.SizeCM, &r.GSM,
			&r.BF, &r.Colour, &r.WeightKg, &r.Date, &r.Time, &r.Quality,
			&r.Party, &r.DispatchDate, &r.DispatchTime, &r.BilledDate, &r.BilledTime,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

//	undo billing to reels
//
// moveBillingToReels handles moving a record from 'billing' back to 'reels' (Undispatch)
func moveBillingToReels(reelID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var br BillingReel
	err = tx.QueryRow(`SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party FROM billing WHERE reel_id = ?`, reelID).
		Scan(&br.ID, &br.ReelID, &br.MonthCode, &br.ItemNumber, &br.SizeCM, &br.GSM, &br.BF, &br.Colour, &br.WeightKg, &br.Date, &br.Time, &br.Quality, &br.Party)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("reel %q not found in billing", reelID)
		}
		return err
	}

	_, err = tx.Exec(`INSERT INTO reels (reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		br.ReelID, br.MonthCode, br.ItemNumber, br.SizeCM, br.GSM, br.BF, br.Colour, br.WeightKg, br.Date, br.Time, br.Quality, br.Party)
	if err != nil {
		return fmt.Errorf("failed to insert back into reels: %w", err)
	}

	_, err = tx.Exec(`DELETE FROM billing WHERE reel_id = ?`, reelID)
	if err != nil {
		return fmt.Errorf("failed to remove from billing: %w", err)
	}

	return tx.Commit()
}

// reel removal
func removeReel(id int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op if committed

	// Look up the reel's matching key (item_number + month_code) before deleting it.
	var itemNumber int
	var monthCode string
	err = tx.QueryRow(`SELECT item_number, month_code FROM reels WHERE id = ?`, id).
		Scan(&itemNumber, &monthCode)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no reel record found with id %d", id)
	}
	if err != nil {
		return err
	}

	// Find every cell whose items array contains an entry for this reel.
	rows, err := tx.Query(`
		SELECT c.id, c.items
		FROM cells c, json_each(c.items) j
		WHERE json_extract(j.value, '$.number') = ?
		  AND json_extract(j.value, '$.month_code') = ?
	`, itemNumber, monthCode)
	if err != nil {
		return err
	}

	type cellRow struct {
		id    int
		items string
	}
	var affected []cellRow
	for rows.Next() {
		var cr cellRow
		if err := rows.Scan(&cr.id, &cr.items); err != nil {
			rows.Close()
			return err
		}
		affected = append(affected, cr)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// Strip the matching item out of each affected cell's items array.
	for _, cr := range affected {
		var items []map[string]interface{}
		if err := json.Unmarshal([]byte(cr.items), &items); err != nil {
			return fmt.Errorf("failed to parse items for cell %d: %w", cr.id, err)
		}

		filtered := items[:0]
		for _, it := range items {
			// JSON numbers unmarshal into float64 via map[string]interface{}
			numF, okNum := it["number"].(float64)
			mc, okMC := it["month_code"].(string)

			if okNum && okMC && int(numF) == itemNumber && mc == monthCode {
				continue // drop this reel's entry
			}
			filtered = append(filtered, it)
		}

		newItems, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("failed to encode items for cell %d: %w", cr.id, err)
		}

		if _, err := tx.Exec(`UPDATE cells SET items = ? WHERE id = ?`, string(newItems), cr.id); err != nil {
			return fmt.Errorf("failed to update cell %d: %w", cr.id, err)
		}
	}

	// Now delete the reel itself.
	res, err := tx.Exec(`DELETE FROM reels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no reel record found with id %d", id)
	}

	return tx.Commit()
}

func modifyReel(r Reel) error {
	res, err := db.Exec(`UPDATE reels SET reel_id = ?, month_code = ?, item_number = ?, size_cm = ?, gsm = ?,
			bf = ?, colour = ?, weight_kg = ?, date = ?, time = ?, quality = ?, party = ? WHERE id = ?`,
		r.ReelID, r.MonthCode, r.ItemNumber, r.SizeCM, r.GSM, r.BF, r.Colour, r.WeightKg, r.Date, r.Time, r.Quality, r.Party, r.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no reel record found with id %d", r.ID)
	}
	return nil
}

func getReel(id int) (*Reel, error) {
	var r Reel
	err := db.QueryRow(`SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party
		FROM reels WHERE id = ?`, id).
		Scan(&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber, &r.SizeCM, &r.GSM, &r.BF, &r.Colour, &r.WeightKg, &r.Date, &r.Time, &r.Quality, &r.Party)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// getAllReels returns reels ordered by recency (most recently added first),
// windowed by [start, end] (both inclusive, 0-based): index 0 is the very
func getAllReels(start, end int) ([]Reel, error) {
	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	if start > end {
		start, end = end, start
	}

	// Calculate limit safely without integer overflow.
	// In SQLite, LIMIT -1 means "return all matching rows from OFFSET".
	limit := -1
	if end < math.MaxInt32-1 {
		limit = end - start + 1
	}

	var rows *sql.Rows
	var err error

	if limit < 0 {
		rows, err = db.Query(`
			SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party
			FROM reels 
			ORDER BY id DESC 
			LIMIT -1 OFFSET ?`, start)
	} else {
		rows, err = db.Query(`
			SELECT id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party
			FROM reels 
			ORDER BY id DESC 
			LIMIT ? OFFSET ?`, limit, start)
	}

	if err != nil {
		return nil, fmt.Errorf("query error in getAllReels: %w", err)
	}
	defer rows.Close()

	out := make([]Reel, 0)
	for rows.Next() {
		var r Reel
		if err := rows.Scan(
			&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber,
			&r.SizeCM, &r.GSM, &r.BF, &r.Colour,
			&r.WeightKg, &r.Date, &r.Time, &r.Quality, &r.Party,
		); err != nil {
			return nil, fmt.Errorf("scan error in getAllReels: %w", err)
		}
		out = append(out, r)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// allReels is a convenience wrapper for callers (search, UI listings) that
// just want the entire reel table, most-recent first.
func allReels() ([]Reel, error) {
	return getAllReels(0, math.MaxInt32)
}

// ---------------------------------------------------------------------------
// Party names — a picklist of party names (with an optional note) that a
// Reel record's Party field can be chosen from.
// ---------------------------------------------------------------------------

func addParty(p Party) (int64, error) {
	if p.Name == "" {
		return 0, fmt.Errorf("party name is required")
	}
	res, err := db.Exec(`INSERT INTO parties (name, note) VALUES (?, ?)`, p.Name, p.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func removeParty(id int) error {
	res, err := db.Exec(`DELETE FROM parties WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no party found with id %d", id)
	}
	return nil
}

func modifyParty(p Party) error {
	if p.Name == "" {
		return fmt.Errorf("party name is required")
	}
	res, err := db.Exec(`UPDATE parties SET name = ?, note = ? WHERE id = ?`, p.Name, p.Note, p.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no party found with id %d", p.ID)
	}
	return nil
}

func getParty(id int) (*Party, error) {
	var p Party
	err := db.QueryRow(`SELECT id, name, note FROM parties WHERE id = ?`, id).Scan(&p.ID, &p.Name, &p.Note)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func getAllParties() ([]Party, error) {
	rows, err := db.Query(`SELECT id, name, note FROM parties ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Party
	for rows.Next() {
		var p Party
		if err := rows.Scan(&p.ID, &p.Name, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Password hashing
// ---------------------------------------------------------------------------

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func checkPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// authenticate validates a user id + auth key pair (the key handed back by
// /api/login) against the DB. A successful call refreshes the key's
// last_used timestamp — this is what keeps an actively-used key alive, since
// cleanupStaleAuthKeys() reaps anything not used in authKeyIdleDays days.
// Returns the user record on success.
func authenticate(userID int, authKey string) (*User, error) {
	if authKey == "" {
		return nil, fmt.Errorf("invalid credentials")
	}
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM auth_keys WHERE key = ? AND user_id = ?`, authKey, userID).Scan(&count)
	if err != nil || count == 0 {
		return nil, fmt.Errorf("invalid credentials")
	}
	u, err := getUserByID(userID)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`UPDATE auth_keys SET last_used = ? WHERE key = ?`, now, authKey)
	return u, nil
}

// authenticateAdmin validates a user id + auth key pair the same way as
// authenticate, but additionally requires that account to be an admin —
// used by endpoints that require full admin rights (e.g. table set).
func authenticateAdmin(userID int, authKey string) (*User, error) {
	u, err := authenticate(userID, authKey)
	if err != nil {
		return nil, err
	}
	if !u.IsAdmin {
		return nil, fmt.Errorf("admin privileges required")
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// Logging: writes to both the "logs" DB table and logs.txt, both capped at
// maxLogLines entries (oldest entries dropped first).
// ---------------------------------------------------------------------------

var logMu sync.Mutex

func logAction(username, action, detail string) {
	logMu.Lock()
	defer logMu.Unlock()

	ts := time.Now().Format("2006-01-02 15:04:05")

	// DB log
	if db != nil {
		_, _ = db.Exec(`INSERT INTO logs (ts, username, action, detail) VALUES (?, ?, ?, ?)`,
			ts, username, action, detail)
		_, _ = db.Exec(`DELETE FROM logs WHERE id NOT IN (
			SELECT id FROM logs ORDER BY id DESC LIMIT ?)`, maxLogLines)
	}

	// Text file log
	line := fmt.Sprintf("[%s] user=%s action=%s detail=%s", ts, username, action, detail)
	appendCappedLine(logFile, line)
}

// appendCappedLine appends a line to path, keeping only the last maxLogLines
// lines in the file.
func appendCappedLine(path, line string) {
	var lines []string
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		f.Close()
	}
	lines = append(lines, line)
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	out, err := os.Create(path)
	if err != nil {
		return
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	w.Flush()
}

func readLogFile() string {
	b, err := os.ReadFile(logFile)
	if err != nil {
		return "(no log entries yet)"
	}
	return string(b)
}

func readAPILogsFromDB() string {
	rows, err := db.Query(`SELECT ts, username, action, detail FROM logs ORDER BY id DESC LIMIT ?`, maxLogLines)
	if err != nil {
		return "(no log entries yet)"
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var ts, username, action, detail string
		if err := rows.Scan(&ts, &username, &action, &detail); err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s] user=%s action=%s detail=%s\n", ts, username, action, detail))
	}
	return sb.String()
}

// Unassigned reels handler

// getUnassignedReels returns all active reels from the database that are NOT
// currently placed inside any cell across any table.
func getUnassignedReels() ([]Reel, error) {
	query := `
		SELECT r.id, r.reel_id, r.month_code, r.item_number, r.size_cm, r.gsm, r.bf, r.colour, r.weight_kg, r.date, r.time, r.quality, r.party
		FROM reels r
		WHERE NOT EXISTS (
			SELECT 1 
			FROM cells c, json_each(c.items) j
			WHERE json_extract(j.value, '$.number') = r.item_number
			  AND json_extract(j.value, '$.month_code') = r.month_code
		)
		ORDER BY r.id DESC;
	`
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	unassigned := make([]Reel, 0)
	for rows.Next() {
		var r Reel
		if err := rows.Scan(&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber, &r.SizeCM, &r.GSM, &r.BF, &r.Colour, &r.WeightKg, &r.Date, &r.Time, &r.Quality, &r.Party); err != nil {
			return nil, err
		}
		unassigned = append(unassigned, r)
	}
	return unassigned, nil
}

// ---------------------------------------------------------------------------
// Request / response payloads
// ---------------------------------------------------------------------------

type errResp struct {
	Error string `json:"error"`
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResp struct {
	UserID  int    `json:"user_id"`
	IsAdmin bool   `json:"is_admin"`
	AuthKey string `json:"auth_key,omitempty"`
}

// moveReq now carries a table id for both ends of the move — an item can
// move within one table or from one physical table to another (e.g.
// re-racking a reel), so source and destination each need their own id.
type moveReq struct {
	UserID      int    `json:"user_id"`
	AuthKey     string `json:"auth_key"`
	Item        int    `json:"item"`
	FromTableID string `json:"from_table_id"`
	FromRow     int    `json:"from_row"`
	FromCol     int    `json:"from_col"`
	ToTableID   string `json:"to_table_id"`
	ToRow       int    `json:"to_row"`
	ToCol       int    `json:"to_col"`
	ToX         int    `json:"to_x"` // stack column of the destination slot
	ToY         int    `json:"to_y"` // stack level of the destination slot
}

type removeReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Item    int    `json:"item"`
	TableID string `json:"table_id"`
	Row     int    `json:"row"`
	Col     int    `json:"col"`
}

type addReq struct {
	UserID    int    `json:"user_id"`
	AuthKey   string `json:"auth_key"`
	Item      int    `json:"item"`
	MonthCode string `json:"month_code"`
	TableID   string `json:"table_id"`
	Row       int    `json:"row"`
	Col       int    `json:"col"`
	X         int    `json:"x"` // stack column within the cell
	Y         int    `json:"y"` // stack level within the cell
}

// tableSetReq requires an admin's id + auth key (checked via
// authenticateAdmin). It replaces every cell belonging to TableID; every
// other table's cells are left untouched.
type tableSetReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	TableID string `json:"table_id"`
	Cells   []Cell `json:"cells"`
}

// cellSetReq lets a client resend a cell's full item list — rearranged,
// trimmed, or otherwise modified — rather than issuing many add/remove/move
// calls. Any authenticated user may call this (not admin-only).
type cellSetReq struct {
	UserID  int        `json:"user_id"`
	AuthKey string     `json:"auth_key"`
	TableID string     `json:"table_id"`
	Row     int        `json:"row"`
	Col     int        `json:"col"`
	Items   []CellItem `json:"items"`
}

// tableCreateReq / tableUpdateReq / tableDeleteReq manage rows in the
// `tables` table itself (physical tables/regions) — distinct from
// tableSetReq, which manages one table's cell contents. All three require
// an admin's id + auth key (checked via authenticateAdmin).
type tableCreateReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Table   Table  `json:"table"`
}

type tableUpdateReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Table   Table  `json:"table"`
}

type tableDeleteReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	ID      string `json:"id"`
}

// cellSizeResp reports table t's item-stack shape; StackType tells the
// client whether Cols applies to every level (vertical) or only to level 0,
// tapering by one per level up to Rows levels total (horizontal).
type cellSizeResp struct {
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	StackType string `json:"stack_type"`
}

// monthCodeAddReq / monthCodeRemoveReq manage the global month code list.
// Requires an admin's id + auth key (checked via authenticateAdmin).
type monthCodeAddReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Code    string `json:"code"`
}

type monthCodeRemoveReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	ID      int    `json:"id"`
}

type reelAddReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Reel    Reel   `json:"reel"`
}

type reelModifyReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Reel    Reel   `json:"reel"`
}

type reelRemoveReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	ID      int    `json:"id"`
}

type partyAddReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Party   Party  `json:"party"`
}

type partyModifyReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	Party   Party  `json:"party"`
}

type partyRemoveReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	ID      int    `json:"id"`
}

// ---------------------------------------------------------------------------
// Change broadcasting — every mutating handler (and the equivalent desktop
// UI actions) calls broadcastEvent() after a successful write. Any client
// holding an open GET /api/events (Server-Sent Events) connection receives
// the event immediately and can replicate the change locally instead of
// re-fetching the whole table.
// ---------------------------------------------------------------------------

var (
	eventSubscribers   = map[chan ChangeEvent]bool{}
	eventSubscribersMu sync.Mutex
)

func subscribeEvents() chan ChangeEvent {
	ch := make(chan ChangeEvent, 32)
	eventSubscribersMu.Lock()
	eventSubscribers[ch] = true
	eventSubscribersMu.Unlock()
	return ch
}

func unsubscribeEvents(ch chan ChangeEvent) {
	eventSubscribersMu.Lock()
	if _, ok := eventSubscribers[ch]; ok {
		delete(eventSubscribers, ch)
		close(ch)
	}
	eventSubscribersMu.Unlock()
}

// broadcastEvent fans a change out to every connected /api/events client.
// Slow/stuck subscribers are skipped (buffered channel, non-blocking send)
// so one bad client can never stall the rest of the server.
func broadcastEvent(eventType, username string, detail interface{}) {
	ev := ChangeEvent{
		EventTime: time.Now().Format(time.RFC3339),
		Type:      eventType,
		Username:  username,
		Detail:    detail,
	}
	eventSubscribersMu.Lock()
	defer eventSubscribersMu.Unlock()
	for ch := range eventSubscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

//---------------------------------------------------------------------------
// Middleware: requireAuth(adminOnly) checks the X-User-ID and X-Auth-Key
// headers, authenticates the user, and optionally requires admin privileges.
// If authentication fails, it returns a 401 Unauthorized response.

func requireAuth(adminOnly bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, err := strconv.Atoi(c.Get("X-User-ID"))
		if err != nil || userID <= 0 {
			return c.Status(fiber.StatusUnauthorized).JSON(errResp{Error: "missing or invalid X-User-ID header"})
		}
		authKey := c.Get("X-Auth-Key")
		if authKey == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(errResp{Error: "missing X-Auth-Key header"})
		}

		var u *User
		if adminOnly {
			u, err = authenticateAdmin(userID, authKey)
		} else {
			u, err = authenticate(userID, authKey)
		}

		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(errResp{Error: err.Error()})
		}

		c.Locals("user", u)
		return c.Next()
	}
}

// ---------------------------------------------------------------------------
// Server lifecycle
// ---------------------------------------------------------------------------

func buildFiberApp() *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: err.Error()})
		},
	})
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("Hello, World!") })
	app.Static("/public", "./public")

	// Unauthenticated Public Routes
	app.Post("/api/login", handleLogin)
	app.Get("/lookupqr", handleQRCodeLookup) // Public API using query parameters

	// User Authenticated Routes (Requires X-User-ID & X-Auth-Key)
	userAPI := app.Group("/api", requireAuth(false))
	userAPI.Get("/validatelogin", handleValidateLogin)
	userAPI.Get("/table", handleGetTable)
	userAPI.Get("/cellsize", handleGetCellSize)
	userAPI.Post("/move", handleMove)
	userAPI.Post("/remove", handleRemove)
	userAPI.Post("/add", handleAdd)
	userAPI.Post("/cellset", handleCellSet)
	userAPI.Get("/tables", handleListTables)
	userAPI.Get("/map", handleGetMap)
	userAPI.Get("/monthcodes", handleGetMonthCodes)
	userAPI.Get("/events", handleEvents)
	userAPI.Get("/reels", handleGetReels)
	userAPI.Post("/reel/add", handleReelAdd)
	userAPI.Post("/reel/remove", handleReelRemove)
	userAPI.Post("/reel/modify", handleReelModify)
	userAPI.Get("/reels/unassigned", handleGetUnassignedReels)
	userAPI.Get("/parties", handleGetParties)
	userAPI.Post("/party/add", handlePartyAdd)
	userAPI.Post("/party/remove", handlePartyRemove)
	userAPI.Post("/party/modify", handlePartyModify)
	userAPI.Post("/search", handleSearch)
	userAPI.Get("/search/meta", handleSearchMeta)
	userAPI.Get("/qrcode", handleQRCode)
	userAPI.Post("/dispatch/add", handleDispatchAdd)
	userAPI.Post("/billing/archive", handleBillingArchive)
	userAPI.Get("/billing", handleGetBilling)
	userAPI.Get("/billed_archive", handleGetArchive)
	userAPI.Post("/dispatch/undo", handleDispatchUndo)

	// Admin Authenticated Routes (Requires Admin via Headers)
	adminAPI := app.Group("/api", requireAuth(true))
	adminAPI.Post("/tableset", handleTableSet)
	adminAPI.Post("/table/create", handleCreateTable)
	adminAPI.Post("/table/update", handleUpdateTable)
	adminAPI.Post("/table/delete", handleDeleteTable)
	adminAPI.Post("/monthcode/add", handleMonthCodeAdd)
	adminAPI.Post("/monthcode/remove", handleMonthCodeRemove)

	app.Use(compress.New(compress.Config{
		Level: compress.LevelBestSpeed,
	}))

	return app
}

// startServer boots the fiber server on the given port over HTTPS and keeps
// running until stopServer() is called (survives the main window being
// hidden). TLS is provided one of two ways:
//
//   - If a public domain name is configured (Server Info -> TLS Domain),
//     certmagic — the same automatic-HTTPS library that powers Caddy —
//     manages a real, browser-trusted certificate via Let's Encrypt,
//     including renewal. This requires the domain to resolve to this
//     machine and ports 80/443 reachable for the ACME HTTP-01 challenge.
//   - Otherwise, a self-signed certificate is generated (once) for
//     "localhost" and this machine's LAN IP and reused across restarts.
//     Public CAs cannot issue trusted certificates for bare IP addresses,
//     so for LAN-only access this is the standard approach: clients must
//     accept/trust the certificate once (browsers will show a warning the
//     first time; a fingerprint is logged to help verify it).
func startServer(port string) {
	serverMu.Lock()
	if serverUp {
		serverMu.Unlock()
		return
	}
	serverMu.Unlock()

	// Ports below 1024 ("privileged" ports, e.g. 80/443) require
	// Administrator on Windows or root on Linux/macOS. Rather than fail
	// with an opaque "permission denied" bind error, check up front and
	// offer to relaunch elevated. This applies even in localhost mode —
	// the restriction is about the port number, not who can reach it.
	if isPrivilegedPort(port) && !isElevated() {
		requestElevationForPort(port)
		return
	}

	serverMu.Lock()
	fiberApp = buildFiberApp()
	serverPort = port
	serverMu.Unlock()

	localhostMode := getSetting("localhost_mode") == "true"

	var ln net.Listener
	var usedDomain string

	if localhostMode {
		// Loopback-only, plain HTTP: never leaves this machine, so there's
		// nothing to encrypt and no certificate for other apps/browsers to
		// trust or complain about — "localhost" is inherently trusted.
		var err error
		ln, err = listenLoopback(port)
		if err != nil {
			logAction("system", "server_error", "listen failed: "+err.Error())
			return
		}
		serverScheme = "http"
		serverAddr = fmt.Sprintf("localhost:%s", port)
	} else {
		tlsConfig, domain, err := buildTLSConfig()
		if err != nil {
			logAction("system", "server_error", "tls setup failed: "+err.Error())
			return
		}
		usedDomain = domain

		rawLn, err := listenPreferIPv6(port)
		if err != nil {
			logAction("system", "server_error", "listen failed: "+err.Error())
			return
		}
		ln = tls.NewListener(rawLn, tlsConfig)
		serverScheme = "https"

		if usedDomain != "" {
			serverAddr = fmt.Sprintf("%s:%s", usedDomain, port)
		} else {
			host, isV6 := localIP()
			if isV6 {
				host = "[" + host + "]"
			}
			serverAddr = fmt.Sprintf("%s:%s", host, port)
		}
	}

	serverMu.Lock()
	serverUp = true
	serverMu.Unlock()
	updateTrayIcon()
	logAction("system", "server_start", fmt.Sprintf("listening on %s://%s (mode=%s)", serverScheme, serverAddr, serverModeDescription(localhostMode, usedDomain)))

	// Blocks until Shutdown() is called.
	if err := fiberApp.Listener(ln); err != nil {
		logAction("system", "server_error", err.Error())
	}

	serverMu.Lock()
	serverUp = false
	serverMu.Unlock()
	updateTrayIcon()
}

// listenLoopback binds to the loopback interface only (IPv6 ::1 preferred,
// IPv4 127.0.0.1 as fallback) — used for localhost-only mode, where the
// server must never be reachable from outside this machine.
func listenLoopback(port string) (net.Listener, error) {
	if ln, err := net.Listen("tcp", "[::1]:"+port); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:"+port)
}

// listenPreferIPv6 binds a dual-stack IPv6 wildcard listener (which also
// accepts IPv4 connections on most platforms) and falls back to an
// IPv4-only wildcard if the system has no usable IPv6 stack.
func listenPreferIPv6(port string) (net.Listener, error) {
	if ln, err := net.Listen("tcp", "[::]:"+port); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "0.0.0.0:"+port)
}

// ---------------------------------------------------------------------------
// Privileged port handling — binding to a port below 1024 needs
// Administrator on Windows or root on Linux/macOS. These helpers detect
// that up front and offer to relaunch the app elevated, rather than
// failing with an opaque "permission denied" bind error.
// ---------------------------------------------------------------------------

func isPrivilegedPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 1024
}

// isElevated reports whether the current process already has the
// privileges needed to bind a port below 1024.
func isElevated() bool {
	if runtime.GOOS == "windows" {
		// "net session" only succeeds when run as Administrator; this is
		// a standard, dependency-free way to check elevation on Windows.
		return exec.Command("net", "session").Run() == nil
	}
	// Unix-like: root has effective UID 0. os.Geteuid() is a portable
	// stdlib call — it simply returns -1 on Windows, never used there.
	return os.Geteuid() == 0
}

// requestElevationForPort asks the user (via a confirm dialog) whether to
// relaunch the app with elevated privileges so it can bind to a port below
// 1024. If they agree and the relaunch is kicked off successfully, this
// process quits, leaving the new elevated instance to start the server
// (it reads the saved port from settings on launch).
func requestElevationForPort(port string) {
	fyne.Do(func() {
		var howTo string
		if runtime.GOOS == "windows" {
			howTo = "Click Relaunch to restart Inventory Manager as Administrator (a UAC prompt will appear)."
		} else {
			howTo = "Click Relaunch to restart Inventory Manager with elevated privileges (via pkexec), " +
				"or close the app and start it manually with sudo."
		}
		msg := fmt.Sprintf("Port %s is below 1024 and requires administrator/root privileges.\n\n%s", port, howTo)
		dialog.ShowConfirm("Administrator Privileges Required", msg, func(ok bool) {
			if !ok {
				logAction("system", "elevation_declined", "port "+port)
				return
			}
			if relaunchElevated() {
				logAction("system", "elevation_relaunch", "relaunching elevated for port "+port)
				fyneApp.Quit()
				return
			}
			dialog.ShowError(fmt.Errorf(
				"could not relaunch with elevated privileges automatically; "+
					"please restart the app as Administrator (Windows) or with sudo (Linux)"), mainWindow)
		}, mainWindow)
	})
}

// relaunchElevated attempts to start a new, elevated copy of this process.
// Returns true if the relaunch was kicked off (the caller should then quit
// this instance); false if elevation isn't available/failed to start.
func relaunchElevated() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		// PowerShell's Start-Process -Verb RunAs triggers the standard UAC
		// elevation prompt for the new process.
		psCmd := fmt.Sprintf("Start-Process -FilePath '%s' -Verb RunAs", exe)
		return exec.Command("powershell", "-Command", psCmd).Start() == nil
	case "linux":
		if pkexec, err := exec.LookPath("pkexec"); err == nil {
			return exec.Command(pkexec, exe).Start() == nil
		}
		return false
	default:
		return false
	}
}

func serverModeDescription(localhostMode bool, domain string) string {
	if localhostMode {
		return "localhost-only, plain HTTP"
	}
	if domain != "" {
		return "automatic (certmagic/Let's Encrypt, domain=" + domain + ")"
	}
	return "self-signed (LAN/IP)"
}

// tlsMode is kept as a thin alias for serverModeDescription for callers
// that only care about the TLS side (never localhost mode).
func tlsMode(domain string) string {
	return serverModeDescription(false, domain)
}

// buildTLSConfig returns the *tls.Config the server should use, and the
// domain name actually used (empty string if falling back to self-signed).
func buildTLSConfig() (*tls.Config, string, error) {
	domain := getSetting("tls_domain")
	if domain == "" {
		cfg, err := selfSignedTLSConfig()
		return cfg, "", err
	}

	email := getSetting("tls_email")
	certmagic.DefaultACME.Email = email
	certmagic.DefaultACME.Agreed = true

	magic := certmagic.NewDefault()
	if err := magic.ManageSync(context.Background(), []string{domain}); err != nil {
		logAction("system", "tls_acme_failed", fmt.Sprintf("automatic HTTPS for %q failed (%v), falling back to self-signed", domain, err))
		cfg, sErr := selfSignedTLSConfig()
		return cfg, "", sErr
	}
	return magic.TLSConfig(), domain, nil
}

const (
	selfSignedCertPath = "certs/selfsigned_cert.pem"
	selfSignedKeyPath  = "certs/selfsigned_key.pem"
	selfSignedValidity = 2 * 365 * 24 * time.Hour // ~2 years
)

// selfSignedTLSConfig loads a cached self-signed certificate from disk if
// one exists and isn't near expiry, otherwise generates a fresh one for
// "localhost" and this machine's LAN IP and caches it for reuse across
// restarts (so the fingerprint clients trust doesn't change every time).
func selfSignedTLSConfig() (*tls.Config, error) {
	if cert, err := loadCachedSelfSignedCert(); err == nil {
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
	cert, err := generateSelfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("failed to generate self-signed certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func loadCachedSelfSignedCert() (tls.Certificate, error) {
	certPEM, err := os.ReadFile(selfSignedCertPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(selfSignedKeyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return tls.Certificate{}, fmt.Errorf("invalid cached certificate")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, err
	}
	if time.Now().After(parsed.NotAfter.Add(-24 * time.Hour)) {
		return tls.Certificate{}, fmt.Errorf("cached certificate is expired or expiring soon")
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// generateSelfSignedCert creates a new ECDSA self-signed certificate valid
// for "localhost", 127.0.0.1, and this machine's detected LAN IP, then
// caches it to disk under certs/.
func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if lan4 := net.ParseIP(localIPv4()); lan4 != nil {
		ips = append(ips, lan4)
	}
	if v6 := localIPv6(); v6 != "" {
		if lan6 := net.ParseIP(v6); lan6 != nil {
			ips = append(ips, lan6)
		}
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Inventory Manager (self-signed)"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	_ = os.MkdirAll("certs", 0700)
	_ = os.WriteFile(selfSignedCertPath, certPEM, 0600)
	_ = os.WriteFile(selfSignedKeyPath, keyPEM, 0600)

	return tls.X509KeyPair(certPEM, keyPEM)
}

func stopServer() {
	serverMu.Lock()
	running := serverUp
	app := fiberApp
	serverMu.Unlock()
	if running && app != nil {
		_ = app.Shutdown()
		logAction("system", "server_stop", "server stopped by user")
	}
}

func isServerRunning() bool {
	serverMu.Lock()
	defer serverMu.Unlock()
	return serverUp
}

// localIPv4 finds the first non-loopback IPv4 address of this machine, used
// to display a copyable "host:port" once the server is exposed.
func localIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return "127.0.0.1"
}

// localIPv6 finds this machine's non-loopback IPv6 address, preferring a
// global/unique-local unicast address over a link-local one (fe80::...),
// since link-local addresses need a zone index to be reachable from other
// hosts and are less useful for display/advertising. Returns "" if no
// usable IPv6 address is found.
func localIPv6() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	linkLocal := ""
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() != nil {
			continue // skip loopback and anything that's actually IPv4
		}
		if ipNet.IP.To16() == nil {
			continue
		}
		if ipNet.IP.IsLinkLocalUnicast() {
			if linkLocal == "" {
				linkLocal = ipNet.IP.String()
			}
			continue
		}
		return ipNet.IP.String()
	}
	return linkLocal
}

// localIP returns the address this machine should advertise for incoming
// connections: IPv6 by default (global/ULA preferred, link-local as a last
// resort), falling back to IPv4 if no IPv6 address is available at all.
// The bool return indicates whether the address is IPv6 (so callers can
// bracket it for use in a host:port string).
func localIP() (ip string, isV6 bool) {
	if v6 := localIPv6(); v6 != "" {
		return v6, true
	}
	return localIPv4(), false
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// Refactored Fiber Handlers (using requireAuth middleware via c.Locals)
// ---------------------------------------------------------------------------

func handleLogin(c *fiber.Ctx) error {
	var req loginReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.Username == "" || req.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "username and password are required"})
	}
	u, err := getUserByUsername(req.Username)
	if err != nil || !checkPassword(u.PasswordHash, req.Password) {
		logAction(req.Username, "login_failed", "invalid credentials from "+c.IP())
		return c.JSON(loginResp{UserID: -1, IsAdmin: false})
	}
	key, err := createAuthKey(u.ID, u.Username)
	if err != nil {
		logAction(u.Username, "login_failed", "auth key generation failed: "+err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to generate auth key"})
	}
	logAction(u.Username, "login_success", "from "+c.IP())
	return c.JSON(loginResp{UserID: u.ID, IsAdmin: u.IsAdmin, AuthKey: key})
}

func handleValidateLogin(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"valid": true})
}

func handleGetTable(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	tableID := c.Query("table_id")
	if tableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table_id query parameter is required"})
	}
	if _, terr := getTable(tableID); terr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: fmt.Sprintf("no table found with id %q", tableID)})
	}
	cells, err := getAllCells(tableID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to read table: " + err.Error()})
	}
	//logAction(u.Username, "get_table", fmt.Sprintf("table_id=%s from %s", tableID, c.IP()))
	return c.JSON(cells)
}

func handleGetCellSize(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	tableID := c.Query("table_id")
	if tableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table_id query parameter is required"})
	}
	t, terr := getTable(tableID)
	if terr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: fmt.Sprintf("no table found with id %q", tableID)})
	}
	//logAction(u.Username, "get_cellsize", fmt.Sprintf("table_id=%s from %s", tableID, c.IP()))
	return c.JSON(cellSizeResp{Cols: t.StackCols, Rows: t.StackRows, StackType: t.StackType})
}

func handleMove(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req moveReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.FromTableID == "" || req.ToTableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "from_table_id and to_table_id are required"})
	}
	toTable, terr := getTable(req.ToTableID)
	if terr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: fmt.Sprintf("no table found with id %q", req.ToTableID)})
	}
	if err := moveItem(req.FromTableID, req.FromRow, req.FromCol, toTable, req.ToRow, req.ToCol, req.Item, req.ToX, req.ToY); err != nil {
		logAction(u.Username, "move_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "move", fmt.Sprintf("item=%d table=%s(%d,%d)->table=%s(%d,%d) x=%d y=%d",
		req.Item, req.FromTableID, req.FromRow, req.FromCol, req.ToTableID, req.ToRow, req.ToCol, req.ToX, req.ToY))
	broadcastEvent("move", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleRemove(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req removeReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.TableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table_id is required"})
	}
	if err := removeItemFromCell(req.TableID, req.Row, req.Col, req.Item); err != nil {
		logAction(u.Username, "remove_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "remove", fmt.Sprintf("item=%d from table=%s (%d,%d)", req.Item, req.TableID, req.Row, req.Col))
	broadcastEvent("remove", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleAdd(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req addReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.TableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table_id is required"})
	}
	t, terr := getTable(req.TableID)
	if terr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: fmt.Sprintf("no table found with id %q", req.TableID)})
	}
	item := CellItem{Number: req.Item, MonthCode: req.MonthCode, X: req.X, Y: req.Y}
	if err := addItemToCell(t, req.Row, req.Col, item); err != nil {
		logAction(u.Username, "add_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "add", fmt.Sprintf("item=%d month_code=%s to table=%s (%d,%d) x=%d y=%d",
		req.Item, req.MonthCode, req.TableID, req.Row, req.Col, req.X, req.Y))
	broadcastEvent("add", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleCellSet(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req cellSetReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.TableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table_id is required"})
	}
	t, terr := getTable(req.TableID)
	if terr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: fmt.Sprintf("no table found with id %q", req.TableID)})
	}
	if err := setCellItems(t, req.Row, req.Col, req.Items); err != nil {
		logAction(u.Username, "cellset_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "cellset", fmt.Sprintf("table=%s cell (%d,%d) now has %d item(s)", req.TableID, req.Row, req.Col, len(req.Items)))
	broadcastEvent("cellset", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleTableSet(c *fiber.Ctx) error {
	admin := c.Locals("user").(*User)
	var req tableSetReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.TableID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table_id is required"})
	}
	t, terr := getTable(req.TableID)
	if terr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: fmt.Sprintf("no table found with id %q", req.TableID)})
	}
	if len(req.Cells) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "cells must contain at least one cell"})
	}
	for i := range req.Cells {
		req.Cells[i].TableID = t.ID
		if req.Cells[i].Items == nil {
			req.Cells[i].Items = []CellItem{}
		}
		if err := validateCellItems(t, req.Cells[i].Items); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: fmt.Sprintf("cell (%d,%d): %s", req.Cells[i].Row, req.Cells[i].Col, err.Error())})
		}
	}
	if err := replaceEntireTableCells(t.ID, req.Cells); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to set table: " + err.Error()})
	}
	logAction(admin.Username, "tableset", fmt.Sprintf("table=%s replaced with %d cells", t.ID, len(req.Cells)))
	broadcastEvent("tableset", admin.Username, fiber.Map{"table_id": t.ID, "cell_count": len(req.Cells)})
	return c.JSON(fiber.Map{"success": true})
}

func handleListTables(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	tables, err := getAllTables()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to read tables: " + err.Error()})
	}
	//logAction(u.Username, "get_tables", fmt.Sprintf("from %s", c.IP()))
	return c.JSON(tables)
}

func handleCreateTable(c *fiber.Ctx) error {
	admin := c.Locals("user").(*User)
	var req tableCreateReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if err := createTable(req.Table); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(admin.Username, "table_create", "id="+req.Table.ID)
	broadcastEvent("table_create", admin.Username, req.Table)
	return c.JSON(fiber.Map{"success": true})
}

func handleUpdateTable(c *fiber.Ctx) error {
	admin := c.Locals("user").(*User)
	var req tableUpdateReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.Table.ID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "table id is required"})
	}
	if err := updateTable(req.Table); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(admin.Username, "table_update", "id="+req.Table.ID)
	broadcastEvent("table_update", admin.Username, req.Table)
	return c.JSON(fiber.Map{"success": true})
}

func handleDeleteTable(c *fiber.Ctx) error {
	admin := c.Locals("user").(*User)
	var req tableDeleteReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if err := deleteTable(req.ID); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(admin.Username, "table_delete", "id="+req.ID)
	broadcastEvent("table_delete", admin.Username, fiber.Map{"id": req.ID})
	return c.JSON(fiber.Map{"success": true})
}

func handleGetMap(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	data, rerr := os.ReadFile(mapSVGFile)
	if rerr != nil {
		return c.Status(fiber.StatusNotFound).JSON(errResp{Error: "no map has been uploaded yet"})
	}
	//logAction(u.Username, "get_map", fmt.Sprintf("from %s", c.IP()))
	c.Set("Content-Type", "image/svg+xml")
	return c.Send(data)
}

func handleGetMonthCodes(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	codes, err := getAllMonthCodes()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to read month codes: " + err.Error()})
	}
	//logAction(u.Username, "get_month_codes", fmt.Sprintf("from %s", c.IP()))
	return c.JSON(codes)
}

func handleMonthCodeAdd(c *fiber.Ctx) error {
	admin := c.Locals("user").(*User)
	var req monthCodeAddReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.Code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "code is required"})
	}
	id, err := addMonthCode(req.Code)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(admin.Username, "monthcode_add", fmt.Sprintf("id=%d code=%s", id, req.Code))
	broadcastEvent("monthcode_add", admin.Username, fiber.Map{"id": id, "code": req.Code})
	return c.JSON(fiber.Map{"success": true, "id": id})
}

func handleMonthCodeRemove(c *fiber.Ctx) error {
	admin := c.Locals("user").(*User)
	var req monthCodeRemoveReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if err := removeMonthCode(req.ID); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(admin.Username, "monthcode_remove", fmt.Sprintf("id=%d", req.ID))
	broadcastEvent("monthcode_remove", admin.Username, fiber.Map{"id": req.ID})
	return c.JSON(fiber.Map{"success": true})
}

func handleGetParties(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	parties, err := getAllParties()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to read parties: " + err.Error()})
	}
	//logAction(u.Username, "get_parties", fmt.Sprintf("from %s", c.IP()))
	return c.JSON(parties)
}

func handlePartyAdd(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req partyAddReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	id, err := addParty(req.Party)
	if err != nil {
		logAction(u.Username, "party_add_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	req.Party.ID = int(id)
	logAction(u.Username, "party_add", fmt.Sprintf("id=%d name=%s", id, req.Party.Name))
	broadcastEvent("party_add", u.Username, req.Party)
	return c.JSON(fiber.Map{"success": true, "id": id})
}

func handlePartyRemove(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req partyRemoveReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if err := removeParty(req.ID); err != nil {
		logAction(u.Username, "party_remove_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "party_remove", fmt.Sprintf("id=%d", req.ID))
	broadcastEvent("party_remove", u.Username, fiber.Map{"id": req.ID})
	return c.JSON(fiber.Map{"success": true})
}

func handlePartyModify(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req partyModifyReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.Party.ID == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "party id is required"})
	}
	if err := modifyParty(req.Party); err != nil {
		logAction(u.Username, "party_modify_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "party_modify", fmt.Sprintf("id=%d", req.Party.ID))
	broadcastEvent("party_modify", u.Username, req.Party)
	return c.JSON(fiber.Map{"success": true})
}

func handleDispatchAdd(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req dispatchAddReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.ReelID == "" || req.DispatchDate == "" || req.DispatchTime == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "reel_id, dispatch_date, and dispatch_time are required"})
	}
	if err := moveReelToBilling(req.ReelID, req.DispatchDate, req.DispatchTime); err != nil {
		logAction(u.Username, "dispatch_add_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "dispatch_add", fmt.Sprintf("reel_id=%s dispatched", req.ReelID))
	broadcastEvent("dispatch_add", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleBillingArchive(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req billedArchiveReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.ReelID == "" || req.BilledDate == "" || req.BilledTime == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "reel_id, billed_date, and billed_time are required"})
	}
	if err := moveBillingToArchive(req.ReelID, req.BilledDate, req.BilledTime); err != nil {
		logAction(u.Username, "billed_archive_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "billed_archive", fmt.Sprintf("reel_id=%s archived", req.ReelID))
	broadcastEvent("billed_archive", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleGetBilling(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	billingReels, err := getAllBilling()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to fetch billing records: " + err.Error()})
	}
	//logAction(u.Username, "get_billing", fmt.Sprintf("fetched %d records from %s", len(billingReels), c.IP()))
	return c.JSON(billingReels)
}

func handleGetArchive(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	limit := c.QueryInt("limit", 100)
	offset := c.QueryInt("offset", 0)
	archiveReels, err := getAllArchive(limit, offset)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to fetch archive records: " + err.Error()})
	}
	//logAction(u.Username, "get_archive", fmt.Sprintf("fetched %d records (offset %d) from %s", len(archiveReels), offset, c.IP()))
	return c.JSON(archiveReels)
}

func handleDispatchUndo(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req dispatchUndoReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if err := moveBillingToReels(req.ReelID); err != nil {
		logAction(u.Username, "dispatch_undo_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "dispatch_undo", fmt.Sprintf("reel_id=%s returned to active reels", req.ReelID))
	broadcastEvent("dispatch_undo", u.Username, req)
	return c.JSON(fiber.Map{"success": true})
}

func handleGetUnassignedReels(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	unassigned, err := getUnassignedReels()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to fetch unassigned reels: " + err.Error()})
	}
	//logAction(u.Username, "get_unassigned_reels", fmt.Sprintf("count=%d from %s", len(unassigned), c.IP()))
	return c.JSON(unassigned)
}

func handleEvents(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	//logAction(u.Username, "subscribe_events", "from "+c.IP())

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("Access-Control-Allow-Origin", "*")

	ch := subscribeEvents()
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer unsubscribeEvents(ch)
		fmt.Fprint(w, ": connected\n\n")
		if err := w.Flush(); err != nil {
			return
		}
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				if err := w.Flush(); err != nil {
					return
				}
			case <-time.After(25 * time.Second):
				fmt.Fprint(w, ": heartbeat\n\n")
				if err := w.Flush(); err != nil {
					return
				}
			}
		}
	})
	return nil
}

func handleGetReels(c *fiber.Ctx) error {
	//u := c.Locals("user").(*User)
	start := c.QueryInt("start", 0)
	end := c.QueryInt("end", math.MaxInt32)
	reels, err := getAllReels(start, end)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to read reels: " + err.Error()})
	}
	//logAction(u.Username, "get_reels", fmt.Sprintf("start=%d end=%d results=%d from %s", start, end, len(reels), c.IP()))
	return c.JSON(reels)
}

func handleReelAdd(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req reelAddReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.Reel.ReelID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "reel_id is required"})
	}
	id, err := addReel(req.Reel)
	if err != nil {
		logAction(u.Username, "reel_add_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	req.Reel.ID = int(id)
	logAction(u.Username, "reel_add", fmt.Sprintf("id=%d reel_id=%s", id, req.Reel.ReelID))
	broadcastEvent("reel_add", u.Username, req.Reel)
	return c.JSON(fiber.Map{"success": true, "id": id})
}

func handleReelRemove(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req reelRemoveReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if err := removeReel(req.ID); err != nil {
		logAction(u.Username, "reel_remove_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "reel_remove", fmt.Sprintf("id=%d", req.ID))
	broadcastEvent("reel_remove", u.Username, fiber.Map{"id": req.ID})
	return c.JSON(fiber.Map{"success": true})
}

func handleReelModify(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req reelModifyReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	if req.Reel.ID == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "reel id is required"})
	}
	if err := modifyReel(req.Reel); err != nil {
		logAction(u.Username, "reel_modify_failed", err.Error())
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: err.Error()})
	}
	logAction(u.Username, "reel_modify", fmt.Sprintf("id=%d", req.Reel.ID))
	broadcastEvent("reel_modify", u.Username, req.Reel)
	return c.JSON(fiber.Map{"success": true})
}

func handleSearch(c *fiber.Ctx) error {
	u := c.Locals("user").(*User)
	var req SearchRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "malformed request body: " + err.Error()})
	}
	target, ok := searchRegistry[strings.ToLower(req.Target)]
	if !ok {
		names := make([]string, 0, len(searchRegistry))
		for k := range searchRegistry {
			names = append(names, k)
		}
		sort.Strings(names)
		return c.Status(fiber.StatusBadRequest).JSON(errResp{
			Error: fmt.Sprintf("unknown search target %q; valid targets: %s", req.Target, strings.Join(names, ", ")),
		})
	}
	for _, f := range req.Filters {
		if f.Field == "" {
			return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "every filter needs a non-empty field"})
		}
	}

	records, err := target.Fetch()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to load data: " + err.Error()})
	}

	filtered := applyFilters(records, req.Filters, req.Match)
	if req.SortField != "" {
		sortRecords(filtered, req.SortField, req.SortOrder)
	}
	total := len(filtered)

	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	page := paginate(filtered, limit, req.Offset)

	logAction(u.Username, "search", fmt.Sprintf("target=%s filters=%d match=%s results=%d/%d", req.Target, len(req.Filters), req.Match, len(page), total))
	return c.JSON(SearchResponse{Target: strings.ToLower(req.Target), Total: total, Count: len(page), Results: page})
}

func handleSearchMeta(c *fiber.Ctx) error {
	ops := []string{"eq", "neq", "contains", "starts_with", "ends_with", "gt", "gte", "lt", "lte", "in", "between"}
	meta := make(fiber.Map, len(searchRegistry))
	for name, t := range searchRegistry {
		meta[name] = fiber.Map{"fields": t.Fields, "ops": ops}
	}
	return c.JSON(meta)
}

var fastPNGEncoder = png.Encoder{
	CompressionLevel: png.BestSpeed,
}

func handleQRCode(c *fiber.Ctx) error {
	text := c.Query("text")
	if text == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errResp{Error: "text query parameter is required"})
	}

	size := c.QueryInt("size", 256)
	if size < 32 {
		size = 32
	} else if size > 2048 {
		size = 2048
	}

	levelStr := c.Query("level", "M") // Defaulting to 'M' saves extra Reed-Solomon CPU cycles
	level := parseRecoveryLevel(levelStr)

	// 1. Generate the raw QR matrix (very fast)
	qr, err := qrcode.New(text, level)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to generate QR matrix: " + err.Error()})
	}

	// 2. Render image matrix
	img := qr.Image(size)

	// 3. Encode to PNG using BestSpeed compression
	var buf bytes.Buffer
	if err := fastPNGEncoder.Encode(&buf, img); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errResp{Error: "failed to encode PNG: " + err.Error()})
	}

	// 4. Non-blocking async logging (goroutine used appropriately for I/O)
	u := c.Locals("user").(*User)
	go logAction(u.Username, "qrcode", fmt.Sprintf("text_len=%d size=%d level=%s", len(text), size, levelStr))

	c.Set("Content-Type", "image/png")
	return c.Send(buf.Bytes())
}

type dispatchUndoReq struct {
	UserID  int    `json:"user_id"`
	AuthKey string `json:"auth_key"`
	ReelID  string `json:"reel_id"`
}

// Add this helper function near handleQRCode
func parseRecoveryLevel(s string) qrcode.RecoveryLevel {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "L":
		return qrcode.Low
	case "M":
		return qrcode.Medium
	case "Q":
		return qrcode.High
	case "H":
		return qrcode.Highest
	default:
		return qrcode.Highest
	}
}

func handleQRCodeLookup(c *fiber.Ctx) error {
	id := c.Query("id")

	// Handle missing ID gracefully
	if id == "" {
		c.Status(fiber.StatusBadRequest)
		return c.Type("html").SendString("<h1>Error: No ID provided</h1>")
	}

	var r BilledArchiveReel // using the largest struct to capture all possible fields
	var sourceTable string

	// Use UNION ALL to search across all three tables, returning only the record that exists.
	query := `
		SELECT 'reels' as source, id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, '' as dispatch_date, '' as dispatch_time, '' as billed_date, '' as billed_time
		FROM reels WHERE reel_id = ?
		UNION ALL
		SELECT 'billing' as source, id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time, '' as billed_date, '' as billed_time
		FROM billing WHERE reel_id = ?
		UNION ALL
		SELECT 'archive' as source, id, reel_id, month_code, item_number, size_cm, gsm, bf, colour, weight_kg, date, time, quality, party, dispatch_date, dispatch_time, billed_date, billed_time
		FROM billed_archive WHERE reel_id = ?
	`

	err := db.QueryRow(query, id, id, id).Scan(
		&sourceTable,
		&r.ID, &r.ReelID, &r.MonthCode, &r.ItemNumber, &r.SizeCM, &r.GSM, &r.BF, &r.Colour, &r.WeightKg, &r.Date, &r.Time, &r.Quality, &r.Party,
		&r.DispatchDate, &r.DispatchTime, &r.BilledDate, &r.BilledTime,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.Status(fiber.StatusNotFound)
			return c.Type("html").SendString(fmt.Sprintf("<h1>Error: Reel #%s not found in any database</h1>", id))
		}
		c.Status(fiber.StatusInternalServerError)
		return c.JSON(errResp{Error: err.Error()})
	}

	// Create dynamic badges based on status
	statusBadge := `<span style="background: #3b82f6; color: white; padding: 4px 8px; border-radius: 4px; font-size: 0.8rem;">IN INVENTORY</span>`
	if sourceTable == "billing" {
		statusBadge = `<span style="background: #eab308; color: white; padding: 4px 8px; border-radius: 4px; font-size: 0.8rem;">DISPATCHED / PENDING BILL</span>`
	} else if sourceTable == "archive" {
		statusBadge = `<span style="background: #22c55e; color: white; padding: 4px 8px; border-radius: 4px; font-size: 0.8rem;">BILLED / ARCHIVED</span>`
	}

	// Dynamic conditional HTML rows
	dispatchHTML := ""
	if sourceTable == "billing" || sourceTable == "archive" {
		dispatchHTML = fmt.Sprintf(`
			<div style="grid-column: span 2; margin-top: 10px; border-top: 1px solid #e2e8f0; padding-top: 10px;">
				<div class="label" style="color: #ea580c;">Dispatch Info</div>
				<div class="value">%s at %s</div>
			</div>`, r.DispatchDate, r.DispatchTime)
	}

	billedHTML := ""
	if sourceTable == "archive" {
		billedHTML = fmt.Sprintf(`
			<div style="grid-column: span 2; margin-top: 10px; border-top: 1px solid #e2e8f0; padding-top: 10px;">
				<div class="label" style="color: #16a34a;">Billed Info</div>
				<div class="value">%s at %s</div>
			</div>`, r.BilledDate, r.BilledTime)
	}

	htmlResponse := fmt.Sprintf(`
		<!DOCTYPE html>
		<html>
		<head>
			<meta name="viewport" content="width=device-width, initial-scale=1.0">
			<style>
				body { font-family: system-ui, sans-serif; background: #f4f6f8; padding: 1.5rem; }
				.card { max-width: 500px; margin: 0 auto; background: #fff; padding: 1.5rem; border-radius: 8px; box-shadow: 0 4px 6px rgba(0,0,0,0.1); }
				h1 { margin-top: 0; color: #1a252f; border-bottom: 2px solid #e2e8f0; padding-bottom: 0.5rem; display: flex; justify-content: space-between; align-items: center; }
				.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0.75rem; }
				.label { font-weight: bold; color: #64748b; font-size: 0.85rem; text-transform: uppercase; }
				.value { font-size: 1.1rem; color: #0f172a; margin-bottom: 0.5rem; }
			</style>
		</head>
		<body>
			<div class="card">
				<h1>
					<span>Reel #%s</span>
					%s
				</h1>
				<div class="grid">
					<div><div class="label">Size (CM)</div><div class="value">%.2f</div></div>
					<div><div class="label">GSM</div><div class="value">%.2f</div></div>
					<div><div class="label">BF</div><div class="value">%s</div></div>
					<div><div class="label">Colour</div><div class="value">%s</div></div>
					<div><div class="label">Weight (KG)</div><div class="value">%.2f</div></div>
					<div><div class="label">Quality</div><div class="value">%s</div></div>
					<div><div class="label">Party</div><div class="value">%s</div></div>
					<div><div class="label">Mfg Date</div><div class="value">%s %s</div></div>
					
					%s
					%s
				</div>
			</div>
		</body>
		</html>
	`,
		r.ReelID, statusBadge,
		r.SizeCM, r.GSM, r.BF, r.Colour, r.WeightKg, r.Quality, r.Party, r.Date, r.Time,
		dispatchHTML, billedHTML,
	)

	return c.Type("html").SendString(htmlResponse)
}

// ---------------------------------------------------------------------------
// Modular search API
//
// A single endpoint, /api/search, can query any registered "target" table
// (cells, items, reels, logs, users) using a programmatic filter builder
// instead of one bespoke endpoint per table. Every target exposes its data
// as []map[string]interface{} (a generic JSON-shaped record), so the same
// filter/sort/paginate engine works across all of them. Adding a new
// searchable table later is a matter of adding one entry to searchRegistry.
// ---------------------------------------------------------------------------

// SearchFilter is one condition in a query, e.g. {"field":"colour","op":"eq","value":"Blue"}.
type SearchFilter struct {
	Field  string      `json:"field"`
	Op     string      `json:"op"` // eq, neq, contains, starts_with, ends_with, gt, gte, lt, lte, in, between
	Value  interface{} `json:"value"`
	Value2 interface{} `json:"value2,omitempty"` // only used by "between"
}

// SearchRequest is the programmatic query itself: pick a target table, a
// list of filters, how filters combine (match "all" = AND, "any" = OR),
// optional sort, and pagination.
type SearchRequest struct {
	UserID    int            `json:"user_id"`
	AuthKey   string         `json:"auth_key"`
	Target    string         `json:"target"`
	Match     string         `json:"match"` // "all" (default) or "any"
	Filters   []SearchFilter `json:"filters"`
	SortField string         `json:"sort_field"`
	SortOrder string         `json:"sort_order"` // "asc" (default) or "desc"
	Limit     int            `json:"limit"`      // default 100, capped at 1000
	Offset    int            `json:"offset"`
}

type SearchResponse struct {
	Target  string                   `json:"target"`
	Total   int                      `json:"total"` // matches before pagination
	Count   int                      `json:"count"` // records in this page
	Results []map[string]interface{} `json:"results"`
}

type searchTarget struct {
	Fields []string
	Fetch  func() ([]map[string]interface{}, error)
}

// searchRegistry lists every table the search API can query. To make a new
// table searchable, add its field names and a Fetch function here — the
// filter/sort/paginate engine below needs no changes.
var searchRegistry = map[string]searchTarget{
	"cells": {
		Fields: []string{"table_id", "row", "col", "color", "number", "items"},
		Fetch:  fetchCellsRecords,
	},
	"items": {
		Fields: []string{"table_id", "row", "col", "cell_color", "cell_number", "item_number", "month_code", "x", "y"},
		Fetch:  fetchItemsRecords,
	},
	"tables": {
		Fields: []string{"id", "name", "grid_rows", "grid_cols", "stack_type", "stack_cols", "stack_rows"},
		Fetch:  fetchTablesRecords,
	},
	"reels": {
		Fields: []string{"id", "reel_id", "month_code", "item_number", "size_cm", "gsm", "bf", "colour", "weight_kg", "date", "time", "quality", "party"},
		Fetch:  fetchReelsRecords,
	},
	"logs": {
		Fields: []string{"ts", "username", "action", "detail"},
		Fetch:  fetchLogsRecords,
	},
	"users": {
		Fields: []string{"id", "username", "is_admin", "created_at"},
		Fetch:  fetchUsersRecords,
	},
	"parties": {
		Fields: []string{"id", "name", "note"},
		Fetch:  fetchPartiesRecords,
	},
	"month_codes": {
		Fields: []string{"id", "code"},
		Fetch:  fetchMonthCodesRecords,
	},
}

func structsToMaps(v interface{}) ([]map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchCellsRecords / fetchItemsRecords search across every table (empty
// tableID means "all tables") since search isn't scoped to one table.
func fetchCellsRecords() ([]map[string]interface{}, error) {
	cells, err := getAllCells("")
	if err != nil {
		return nil, err
	}
	return structsToMaps(cells)
}

// fetchItemsRecords flattens every item out of every cell (across every
// table) into its own record, so items can be searched directly (e.g. "find
// item 1042" or "all items with month_code 2026-07") without knowing which
// table or cell holds them.
func fetchItemsRecords() ([]map[string]interface{}, error) {
	cells, err := getAllCells("")
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0)
	for _, c := range cells {
		for _, it := range c.Items {
			out = append(out, map[string]interface{}{
				"table_id":    c.TableID,
				"row":         c.Row,
				"col":         c.Col,
				"cell_color":  c.Color,
				"cell_number": c.Number,
				"item_number": it.Number,
				"month_code":  it.MonthCode,
				"x":           it.X,
				"y":           it.Y,
			})
		}
	}
	return out, nil
}

func fetchTablesRecords() ([]map[string]interface{}, error) {
	tables, err := getAllTables()
	if err != nil {
		return nil, err
	}
	return structsToMaps(tables)
}

func fetchPartiesRecords() ([]map[string]interface{}, error) {
	parties, err := getAllParties()
	if err != nil {
		return nil, err
	}
	return structsToMaps(parties)
}

func fetchMonthCodesRecords() ([]map[string]interface{}, error) {
	codes, err := getAllMonthCodes()
	if err != nil {
		return nil, err
	}
	return structsToMaps(codes)
}

func fetchReelsRecords() ([]map[string]interface{}, error) {
	reels, err := allReels()
	if err != nil {
		return nil, err
	}
	return structsToMaps(reels)
}

func fetchLogsRecords() ([]map[string]interface{}, error) {
	rows, err := db.Query(`SELECT ts, username, action, detail FROM logs ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var ts, username, action, detail string
		if err := rows.Scan(&ts, &username, &action, &detail); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{"ts": ts, "username": username, "action": action, "detail": detail})
	}
	return out, nil
}

// fetchUsersRecords deliberately omits password_hash — search results are
// never allowed to leak credential material.
func fetchUsersRecords() ([]map[string]interface{}, error) {
	users, err := listUsers()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]interface{}{
			"id": u.ID, "username": u.Username, "is_admin": u.IsAdmin, "created_at": u.CreatedAt,
		})
	}
	return out, nil
}

// --- generic filter/sort/paginate engine, shared by every target ---------

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func compareEqual(a, b interface{}) bool {
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	if as, aok := a.(string); aok {
		if bs, bok := b.(string); bok {
			return strings.EqualFold(as, bs)
		}
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func containsValue(val, query interface{}) bool {
	switch t := val.(type) {
	case string:
		qs, ok := query.(string)
		return ok && strings.Contains(strings.ToLower(t), strings.ToLower(qs))
	case []interface{}:
		for _, item := range t {
			if compareEqual(item, query) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func compareOrdered(val, query interface{}, op string) bool {
	if vf, vok := toFloat(val); vok {
		if qf, qok := toFloat(query); qok {
			switch op {
			case "gt":
				return vf > qf
			case "gte":
				return vf >= qf
			case "lt":
				return vf < qf
			case "lte":
				return vf <= qf
			}
		}
	}
	vs, vok := val.(string)
	qs, qok := query.(string)
	if vok && qok {
		switch op {
		case "gt":
			return vs > qs
		case "gte":
			return vs >= qs
		case "lt":
			return vs < qs
		case "lte":
			return vs <= qs
		}
	}
	return false
}

func compareBetween(val, lo, hi interface{}) bool {
	if vf, vok := toFloat(val); vok {
		if lof, lok := toFloat(lo); lok {
			if hif, hok := toFloat(hi); hok {
				return vf >= lof && vf <= hif
			}
		}
	}
	vs, vok := val.(string)
	los, lok := lo.(string)
	his, hok := hi.(string)
	if vok && lok && hok {
		return vs >= los && vs <= his
	}
	return false
}

func matchFilter(record map[string]interface{}, f SearchFilter) bool {
	val, exists := record[f.Field]
	if !exists {
		return false
	}
	switch strings.ToLower(f.Op) {
	case "eq":
		return compareEqual(val, f.Value)
	case "neq":
		return !compareEqual(val, f.Value)
	case "contains":
		return containsValue(val, f.Value)
	case "starts_with":
		s, ok1 := val.(string)
		q, ok2 := f.Value.(string)
		return ok1 && ok2 && strings.HasPrefix(strings.ToLower(s), strings.ToLower(q))
	case "ends_with":
		s, ok1 := val.(string)
		q, ok2 := f.Value.(string)
		return ok1 && ok2 && strings.HasSuffix(strings.ToLower(s), strings.ToLower(q))
	case "gt", "gte", "lt", "lte":
		return compareOrdered(val, f.Value, strings.ToLower(f.Op))
	case "in":
		list, ok := f.Value.([]interface{})
		if !ok {
			return false
		}
		for _, item := range list {
			if compareEqual(val, item) {
				return true
			}
		}
		return false
	case "between":
		return compareBetween(val, f.Value, f.Value2)
	default:
		return false
	}
}

func applyFilters(records []map[string]interface{}, filters []SearchFilter, match string) []map[string]interface{} {
	if len(filters) == 0 {
		return records
	}
	any := strings.EqualFold(match, "any")
	out := make([]map[string]interface{}, 0, len(records))
	for _, r := range records {
		matched := !any // start true for "all" (AND), false for "any" (OR)
		if any {
			for _, f := range filters {
				if matchFilter(r, f) {
					matched = true
					break
				}
			}
		} else {
			matched = true
			for _, f := range filters {
				if !matchFilter(r, f) {
					matched = false
					break
				}
			}
		}
		if matched {
			out = append(out, r)
		}
	}
	return out
}

func lessValue(a, b interface{}) bool {
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af < bf
		}
	}
	if as, aok := a.(string); aok {
		if bs, bok := b.(string); bok {
			return strings.ToLower(as) < strings.ToLower(bs)
		}
	}
	return fmt.Sprintf("%v", a) < fmt.Sprintf("%v", b)
}

func sortRecords(records []map[string]interface{}, field, order string) {
	if field == "" {
		return
	}
	desc := strings.EqualFold(order, "desc")
	sort.SliceStable(records, func(i, j int) bool {
		if desc {
			return lessValue(records[j][field], records[i][field])
		}
		return lessValue(records[i][field], records[j][field])
	})
}

func paginate(records []map[string]interface{}, limit, offset int) []map[string]interface{} {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(records) {
		return []map[string]interface{}{}
	}
	end := len(records)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return records[offset:end]
}

// wizardTableEntry carries setup-time state for one physical table (region)
// discovered from map.svg, plus the cell grid used by the colour/number
// layout editor (shared with the dashboard's Inventory Grid view).
type wizardTableEntry struct {
	ID        string
	Name      string
	GridRows  int
	GridCols  int
	StackType string // "vertical" or "horizontal" — see the Table doc comment
	StackCols int
	StackRows int
	Cells     [][]Cell // [row][col]; items always start empty during setup
}

// wizardState carries data collected across setup wizard pages.
type wizardState struct {
	adminPassword string
	port          string

	mapSrcPath string // filesystem path the admin picked (informational)
	tables     []*wizardTableEntry
}

var wizard = &wizardState{}

func showSetupWizard() {
	progress := widget.NewProgressBar()
	progress.Min = 0
	progress.Max = 5 // 6 steps, indices 0..5

	stepTitle := widget.NewLabelWithStyle("", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	content := container.NewStack()

	var currentStep int
	var nextBtn, prevBtn *widget.Button

	steps := []struct {
		title string
		build func() fyne.CanvasObject
	}{
		{"Step 1 of 6 — Admin Account", buildAdminStep},
		{"Step 2 of 6 — User Accounts", buildUsersStep},
		{"Step 3 of 6 — Map Upload", buildMapStep},
		{"Step 4 of 6 — Table Parameters", buildTableParamsStep},
		{"Step 5 of 6 — Deploy Server Process", buildDeployStep},
		{"Step 6 of 6 — Expose API Route", buildExposeStep},
	}

	renderStep := func(i int) {
		currentStep = i
		progress.SetValue(float64(i))
		stepTitle.SetText(steps[i].title)
		content.Objects = []fyne.CanvasObject{steps[i].build()}
		content.Refresh()
		prevBtn.Disable()
		if i > 0 {
			prevBtn.Enable()
		}
		if i == len(steps)-1 {
			nextBtn.SetText("Finish")
		} else {
			nextBtn.SetText("Next")
		}
	}

	nextBtn = widget.NewButton("Next", nil)
	prevBtn = widget.NewButton("Previous", nil)

	nextBtn.OnTapped = func() {
		if err := validateStep(currentStep); err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		if currentStep == len(steps)-1 {
			finishSetup()
			return
		}
		renderStep(currentStep + 1)
	}
	prevBtn.OnTapped = func() {
		if currentStep > 0 {
			renderStep(currentStep - 1)
		}
	}

	logo := canvas.NewImageFromResource(loadResource("logo.png", theme.StorageIcon()))
	logo.FillMode = canvas.ImageFillContain
	logo.SetMinSize(fyne.NewSize(48, 48))

	header := container.NewVBox(
		container.NewHBox(logo, widget.NewLabelWithStyle("Inventory Manager Setup", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
		progress,
		stepTitle,
		widget.NewSeparator(),
	)

	footer := container.NewBorder(nil, nil, prevBtn, nextBtn, layout.NewSpacer())

	root := container.NewBorder(header, footer, nil, nil, content)
	mainWindow.SetContent(root)
	renderStep(0)
}

func validateStep(step int) error {
	switch step {
	case 0:
		if wizard.adminPassword == "" {
			return fmt.Errorf("please set an admin password before continuing")
		}
	case 2: // Map Upload
		if len(wizard.tables) == 0 {
			return fmt.Errorf("please upload map.svg and parse it to discover at least one table before continuing")
		}
	case 3: // Table Parameters
		for _, te := range wizard.tables {
			if te.GridRows < 1 || te.GridCols < 1 {
				return fmt.Errorf("table %q: grid must have at least 1 row and 1 column", te.ID)
			}
			if te.StackCols < 1 || te.StackRows < 1 {
				return fmt.Errorf("table %q: stack must have at least 1 column and 1 row", te.ID)
			}
			if te.StackType != "vertical" && te.StackType != "horizontal" {
				return fmt.Errorf("table %q: stacking type must be \"vertical\" or \"horizontal\"", te.ID)
			}
			if te.StackType == "horizontal" && te.StackRows > te.StackCols {
				return fmt.Errorf("table %q: stack rows (%d) cannot exceed stack columns (%d) for horizontal (tapering) stacking",
					te.ID, te.StackRows, te.StackCols)
			}
		}
	case 5: // Expose API Route
		if wizard.port == "" {
			return fmt.Errorf("please choose a port before exposing the API")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Step 1: Admin password
// ---------------------------------------------------------------------------

func buildAdminStep() fyne.CanvasObject {
	usernameLabel := widget.NewLabel("admin")
	pw1 := widget.NewPasswordEntry()
	pw1.SetPlaceHolder("Enter admin password")
	pw2 := widget.NewPasswordEntry()
	pw2.SetPlaceHolder("Confirm admin password")

	if wizard.adminPassword != "" {
		pw1.SetText(wizard.adminPassword)
		pw2.SetText(wizard.adminPassword)
	}

	pw1.OnChanged = func(s string) { wizard.adminPassword = s }
	pw2.OnChanged = func(s string) {
		if s != pw1.Text {
			pw2.SetValidationError(fmt.Errorf("passwords do not match"))
		} else {
			pw2.SetValidationError(nil)
			wizard.adminPassword = s
		}
	}

	form := widget.NewForm(
		widget.NewFormItem("Admin Username", usernameLabel),
		widget.NewFormItem("Password", pw1),
		widget.NewFormItem("Confirm Password", pw2),
	)

	info := widget.NewLabel("This account has full administrative rights: manage users, edit the\ninventory grid, and push table updates via the API.")
	info.Wrapping = fyne.TextWrapWord

	return container.NewVBox(widget.NewCard("Administrator Account", "", form), info)
}

// ---------------------------------------------------------------------------
// Step 2: User accounts
// ---------------------------------------------------------------------------

var wizardUsersList *widget.List
var wizardPendingUsers []struct {
	Username, Password string
}

func buildUsersStep() fyne.CanvasObject {
	unameEntry := widget.NewEntry()
	unameEntry.SetPlaceHolder("Username")
	pwEntry := widget.NewPasswordEntry()
	pwEntry.SetPlaceHolder("Password")

	list := widget.NewList(
		func() int { return len(wizardPendingUsers) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButton("Remove", nil), widget.NewLabel(""))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			btn := row.Objects[1].(*widget.Button)
			label.SetText(wizardPendingUsers[id].Username)
			btn.OnTapped = func() {
				wizardPendingUsers = append(wizardPendingUsers[:id], wizardPendingUsers[id+1:]...)
				wizardUsersList.Refresh()
			}
		},
	)
	wizardUsersList = list
	list.Resize(fyne.NewSize(400, 200))

	addBtn := widget.NewButtonWithIcon("Add User", theme.ContentAddIcon(), func() {
		if unameEntry.Text == "" || pwEntry.Text == "" {
			dialog.ShowError(fmt.Errorf("username and password are required"), mainWindow)
			return
		}
		if unameEntry.Text == "admin" {
			dialog.ShowError(fmt.Errorf("'admin' is reserved for the administrator account"), mainWindow)
			return
		}
		wizardPendingUsers = append(wizardPendingUsers, struct{ Username, Password string }{unameEntry.Text, pwEntry.Text})
		unameEntry.SetText("")
		pwEntry.SetText("")
		wizardUsersList.Refresh()
	})

	form := container.NewBorder(nil, nil, nil, addBtn, container.NewGridWithColumns(2, unameEntry, pwEntry))
	info := widget.NewLabel("Create one or more staff accounts now. You can always add, remove, or\nreset passwords later from the dashboard. This step is optional.")
	info.Wrapping = fyne.TextWrapWord

	listScroll := container.NewVScroll(list)
	listScroll.SetMinSize(fyne.NewSize(400, 220))

	return container.NewVBox(widget.NewCard("Staff Accounts", "", form), listScroll, info)
}

// ---------------------------------------------------------------------------
// Step 3: Map upload — pick map.svg, parse it for table (region) ids
// ---------------------------------------------------------------------------

func buildMapStep() fyne.CanvasObject {
	pathLabel := widget.NewLabel("(no file selected)")
	if wizard.mapSrcPath != "" {
		pathLabel.SetText(wizard.mapSrcPath)
	}

	discovered := widget.NewLabel("")
	discovered.Wrapping = fyne.TextWrapWord
	refreshDiscovered := func() {
		if len(wizard.tables) == 0 {
			discovered.SetText("No tables discovered yet.")
			return
		}
		ids := make([]string, len(wizard.tables))
		for i, te := range wizard.tables {
			ids[i] = te.ID
		}
		discovered.SetText(fmt.Sprintf("Discovered %d table(s): %s", len(ids), strings.Join(ids, ", ")))
	}
	refreshDiscovered()

	chooseBtn := widget.NewButtonWithIcon("Choose map.svg…", theme.FolderOpenIcon(), func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, ferr error) {
			if ferr != nil {
				dialog.ShowError(ferr, mainWindow)
				return
			}
			if rc == nil {
				return // cancelled
			}
			defer rc.Close()
			data, rerr := io.ReadAll(rc)
			if rerr != nil {
				dialog.ShowError(rerr, mainWindow)
				return
			}
			// Stage the file in the working directory right away so parsing
			// (and a preview via GET /api/map) reads the same bytes the
			// admin just picked. finishSetup() doesn't need to re-copy it,
			// but does so defensively in case this step is revisited.
			if werr := os.WriteFile(mapSVGFile, data, 0644); werr != nil {
				dialog.ShowError(fmt.Errorf("failed to stage map file: %w", werr), mainWindow)
				return
			}
			wizard.mapSrcPath = rc.URI().Path()
			pathLabel.SetText(wizard.mapSrcPath)

			ids, perr := parseMapTableIDs(mapSVGFile)
			if perr != nil {
				dialog.ShowError(fmt.Errorf("failed to parse SVG: %w", perr), mainWindow)
				return
			}
			if len(ids) == 0 {
				dialog.ShowError(fmt.Errorf("no filled shapes with an id were found in this SVG (reference points named \"refpoint...\" don't count)"), mainWindow)
				return
			}
			// Preserve parameters for ids already configured (e.g. the admin
			// re-picking a corrected file); add fresh defaults for new ones.
			existing := map[string]*wizardTableEntry{}
			for _, te := range wizard.tables {
				existing[te.ID] = te
			}
			merged := make([]*wizardTableEntry, 0, len(ids))
			for _, id := range ids {
				if te, ok := existing[id]; ok {
					merged = append(merged, te)
					continue
				}
				merged = append(merged, &wizardTableEntry{
					ID: id, Name: id, GridRows: 1, GridCols: 1,
					StackType: "vertical", StackCols: 3, StackRows: 3,
				})
			}
			wizard.tables = merged
			refreshDiscovered()
		}, mainWindow)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".svg"}))
		fd.Show()
	})

	info := widget.NewLabel("Upload the floor-plan SVG for this site. Every filled polygon (or other\n" +
		"shape) in the file becomes one physical table — its SVG id is used as\n" +
		"the table's id. Two small objects named \"refpoint1\" / \"refpoint2\" may\n" +
		"also be present for the frontend to align the drawing; they are not\n" +
		"treated as tables.\n\n" +
		"The file is copied to map.svg next to the app; you can replace it later\n" +
		"from the dashboard's Map view.")
	info.Wrapping = fyne.TextWrapWord

	return container.NewVBox(
		widget.NewCard("Map Upload", "", container.NewVBox(chooseBtn, pathLabel, discovered)),
		info,
	)
}

// ---------------------------------------------------------------------------
// Step 4: Table parameters — size, stacking type, and row length per table
// ---------------------------------------------------------------------------

func buildTableParamsStep() fyne.CanvasObject {
	if len(wizard.tables) == 0 {
		return container.NewVBox(widget.NewLabel("No tables discovered yet — go back to Map Upload first."))
	}

	box := container.NewVBox()
	for _, te := range wizard.tables {
		box.Add(buildTableParamsCard(te))
	}

	scroll := container.NewVScroll(box)
	scroll.SetMinSize(fyne.NewSize(560, 420))
	return scroll
}

func buildTableParamsCard(te *wizardTableEntry) fyne.CanvasObject {
	nameEntry := widget.NewEntry()
	nameEntry.SetText(te.Name)
	nameEntry.OnChanged = func(s string) { te.Name = s }

	rowsEntry := widget.NewEntry()
	rowsEntry.SetText(strconv.Itoa(te.GridRows))
	rowsEntry.OnChanged = func(s string) {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			te.GridRows = n
		}
	}
	colsEntry := widget.NewEntry()
	colsEntry.SetText(strconv.Itoa(te.GridCols))
	colsEntry.OnChanged = func(s string) {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			te.GridCols = n
		}
	}

	stackTypeSelect := widget.NewSelect([]string{"vertical", "horizontal"}, func(s string) { te.StackType = s })
	stackTypeSelect.SetSelected(te.StackType)

	rowLengthEntry := widget.NewEntry()
	rowLengthEntry.SetText(strconv.Itoa(te.StackCols))
	rowLengthEntry.OnChanged = func(s string) {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			te.StackCols = n
		}
	}
	levelsEntry := widget.NewEntry()
	levelsEntry.SetText(strconv.Itoa(te.StackRows))
	levelsEntry.OnChanged = func(s string) {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			te.StackRows = n
		}
	}

	editLayoutBtn := widget.NewButtonWithIcon("Edit Layout / Colours…", theme.GridIcon(), func() {
		showTableLayoutDialog(te)
	})

	form := widget.NewForm(
		widget.NewFormItem("Name", nameEntry),
		widget.NewFormItem("Grid rows", rowsEntry),
		widget.NewFormItem("Grid cols", colsEntry),
		widget.NewFormItem("Stacking type", stackTypeSelect),
		widget.NewFormItem("Row length (slots per stack row)", rowLengthEntry),
		widget.NewFormItem("Stack levels", levelsEntry),
	)

	return widget.NewCard(fmt.Sprintf("Table: %s", te.ID), "", container.NewVBox(form, editLayoutBtn))
}

// showTableLayoutDialog opens the interactive cell colour/number grid
// editor for one table in a modal dialog.
func showTableLayoutDialog(te *wizardTableEntry) {
	ensureTableGridSize(te)
	editor := buildTableGridEditor(te)
	d := dialog.NewCustom(fmt.Sprintf("Layout — %s", te.ID), "Done", editor, mainWindow)
	d.Resize(fyne.NewSize(560, 480))
	d.Show()
}

// ---------------------------------------------------------------------------
// Shared per-table grid editor — an interactive rows×cols grid where each
// cell is a coloured, numbered, clickable rectangle. Used by the setup
// wizard's per-table layout dialog and by the dashboard's Inventory Grid
// view (buildInventoryEditorView).
// ---------------------------------------------------------------------------

// ensureTableGridSize grows/shrinks te.Cells to match te.GridRows/GridCols,
// preserving existing colours where possible and renumbering sequentially.
func ensureTableGridSize(te *wizardTableEntry) {
	for len(te.Cells) < te.GridRows {
		te.Cells = append(te.Cells, []Cell{})
	}
	for r := 0; r < te.GridRows; r++ {
		for len(te.Cells[r]) < te.GridCols {
			idx := len(te.Cells[r])
			te.Cells[r] = append(te.Cells[r], Cell{
				TableID: te.ID,
				Row:     r,
				Col:     idx,
				Color:   colorPalette[(r*te.GridCols+idx)%len(colorPalette)],
				Items:   []CellItem{},
			})
		}
		if len(te.Cells[r]) > te.GridCols {
			te.Cells[r] = te.Cells[r][:te.GridCols]
		}
	}
	if len(te.Cells) > te.GridRows {
		te.Cells = te.Cells[:te.GridRows]
	}
	renumberTableCells(te)
}

func renumberTableCells(te *wizardTableEntry) {
	n := 1
	for r := 0; r < te.GridRows; r++ {
		for c := 0; c < te.GridCols; c++ {
			te.Cells[r][c].TableID = te.ID
			te.Cells[r][c].Row = r
			te.Cells[r][c].Col = c
			te.Cells[r][c].Number = n
			n++
		}
	}
}

// buildTableGridEditor renders an interactive rows×cols grid editor for
// table entry te — add/remove rows and columns, click a cell to recolour
// it.
func buildTableGridEditor(te *wizardTableEntry) fyne.CanvasObject {
	ensureTableGridSize(te)

	rowsLabel := widget.NewLabel(fmt.Sprintf("Rows: %d", te.GridRows))
	colsLabel := widget.NewLabel(fmt.Sprintf("Cols: %d", te.GridCols))

	gridContainer := container.NewGridWithColumns(te.GridCols)
	var refresh func()
	refresh = func() {
		gridContainer.Objects = nil
		gridContainer.Layout = layout.NewGridLayoutWithColumns(te.GridCols)
		for r := 0; r < te.GridRows; r++ {
			for c := 0; c < te.GridCols; c++ {
				gridContainer.Add(buildCellWidget(te, r, c))
			}
		}
		rowsLabel.SetText(fmt.Sprintf("Rows: %d", te.GridRows))
		colsLabel.SetText(fmt.Sprintf("Cols: %d", te.GridCols))
		gridContainer.Refresh()
	}

	addRow := widget.NewButtonWithIcon("", theme.ContentAddIcon(), func() {
		te.GridRows++
		ensureTableGridSize(te)
		refresh()
	})
	removeRow := widget.NewButtonWithIcon("", theme.ContentRemoveIcon(), func() {
		if te.GridRows > 1 {
			te.GridRows--
			te.Cells = te.Cells[:te.GridRows]
			refresh()
		}
	})
	addCol := widget.NewButtonWithIcon("", theme.ContentAddIcon(), func() {
		te.GridCols++
		ensureTableGridSize(te)
		refresh()
	})
	removeCol := widget.NewButtonWithIcon("", theme.ContentRemoveIcon(), func() {
		if te.GridCols > 1 {
			te.GridCols--
			for r := range te.Cells {
				te.Cells[r] = te.Cells[r][:te.GridCols]
			}
			refresh()
		}
	})

	rowControls := container.NewVBox(rowsLabel, addRow, removeRow)
	colControls := container.NewHBox(colsLabel, addCol, removeCol)

	refresh()

	scroll := container.NewScroll(gridContainer)
	scroll.SetMinSize(fyne.NewSize(500, 350))

	info := widget.NewLabel("Click a cell to change its colour. Numbers are assigned automatically\nleft-to-right, top-to-bottom, and update if you resize the grid.")
	info.Wrapping = fyne.TextWrapWord

	body := container.NewBorder(colControls, nil, rowControls, nil, scroll)
	return container.NewVBox(body, info)
}

// buildCellWidget renders one grid cell (of table te) as a coloured,
// numbered, clickable rectangle. Clicking opens a small colour-swatch
// picker.
func buildCellWidget(te *wizardTableEntry, r, c int) fyne.CanvasObject {
	cell := &te.Cells[r][c]
	rect := canvas.NewRectangle(hexToColor(cell.Color))
	rect.SetMinSize(fyne.NewSize(64, 48))
	numberLabel := widget.NewLabelWithStyle(strconv.Itoa(cell.Number), fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	stack := container.NewStack(rect, container.NewCenter(numberLabel))
	button := widget.NewButton("", func() {
		showColorSwatchPicker(func(hex string) {
			cell.Color = hex
			rect.FillColor = hexToColor(hex)
			rect.Refresh()
		})
	})
	button.Importance = widget.LowImportance
	return container.NewStack(stack, button)
}

func showColorSwatchPicker(onPick func(hex string)) {
	var d dialog.Dialog
	swatches := container.NewGridWithColumns(5)
	for _, hex := range colorPalette {
		h := hex
		rect := canvas.NewRectangle(hexToColor(h))
		rect.SetMinSize(fyne.NewSize(36, 36))
		btn := widget.NewButton("", func() {
			onPick(h)
			d.Hide()
		})
		btn.Importance = widget.LowImportance
		swatches.Add(container.NewStack(rect, btn))
	}
	d = dialog.NewCustom("Choose a colour", "Cancel", swatches, mainWindow)
	d.Show()
}

func hexToColor(hex string) color.Color {
	hex = trimHash(hex)
	if len(hex) != 6 {
		return theme.PrimaryColor()
	}
	var r, g, b int64
	fmt.Sscanf(hex[0:2], "%02x", &r)
	fmt.Sscanf(hex[2:4], "%02x", &g)
	fmt.Sscanf(hex[4:6], "%02x", &b)
	return color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
}

func trimHash(s string) string {
	if len(s) > 0 && s[0] == '#' {
		return s[1:]
	}
	return s
}

// ---------------------------------------------------------------------------
// Step 5: Deploy startup / forever-running server process
// ---------------------------------------------------------------------------

func buildDeployStep() fyne.CanvasObject {
	statusLabel := widget.NewLabel("The background server process will start now and will keep running\n(with a system tray icon) even if you close this window.")
	statusLabel.Wrapping = fyne.TextWrapWord

	indicator := widget.NewLabel("Preparing…")
	go func() {
		// Nothing to actually bind yet (port chosen on next step); this just
		// confirms the DB/tray/process are healthy before moving on.
		fyne.Do(func() {
			indicator.SetText("✔ Background process ready. A tray icon is now active.")
		})
	}()

	return container.NewVBox(widget.NewCard("Deploy Server Process", "", container.NewVBox(statusLabel, indicator)))
}

// ---------------------------------------------------------------------------
// Step 6: Expose API route (choose port, start listening)
// ---------------------------------------------------------------------------

func buildExposeStep() fyne.CanvasObject {
	portEntry := widget.NewEntry()
	portEntry.SetPlaceHolder("e.g. 8443")
	if wizard.port != "" {
		portEntry.SetText(wizard.port)
	}
	portEntry.OnChanged = func(s string) { wizard.port = s }

	domainEntry := widget.NewEntry()
	domainEntry.SetPlaceHolder("optional — e.g. inventory.example.com")
	domainEntry.SetText(getSetting("tls_domain"))
	emailEntry := widget.NewEntry()
	emailEntry.SetPlaceHolder("optional — for Let's Encrypt renewal notices")
	emailEntry.SetText(getSetting("tls_email"))
	tlsForm := widget.NewForm(
		widget.NewFormItem("TLS Domain (optional)", domainEntry),
		widget.NewFormItem("Contact Email (optional)", emailEntry),
	)

	localhostCheck := widget.NewCheck("Localhost only — don't expose a network port, no certificate needed", nil)
	localhostCheck.SetChecked(getSetting("localhost_mode") == "true")
	localhostCheck.OnChanged = func(checked bool) {
		if checked {
			tlsForm.Hide()
		} else {
			tlsForm.Show()
		}
	}
	if localhostCheck.Checked {
		tlsForm.Hide()
	}

	resultLabel := widget.NewLabel("")
	resultLabel.Wrapping = fyne.TextWrapWord
	copyBtn := widget.NewButtonWithIcon("Copy Address", theme.ContentCopyIcon(), func() {
		mainWindow.Clipboard().SetContent(serverScheme + "://" + serverAddr)
	})
	copyBtn.Hide()

	startBtn := widget.NewButton("Start Server", func() {
		if _, err := strconv.Atoi(wizard.port); err != nil {
			dialog.ShowError(fmt.Errorf("port must be a number"), mainWindow)
			return
		}
		_ = setSetting("tls_domain", domainEntry.Text)
		_ = setSetting("tls_email", emailEntry.Text)
		if localhostCheck.Checked {
			_ = setSetting("localhost_mode", "true")
		} else {
			_ = setSetting("localhost_mode", "false")
		}
		go startServer(wizard.port)
		go func() {
			// give the listener a brief moment to bind
			for i := 0; i < 40 && !isServerRunning(); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			fyne.Do(func() {
				if isServerRunning() {
					resultLabel.SetText(fmt.Sprintf("Server is live at:\n%s://%s", serverScheme, serverAddr))
					copyBtn.Show()
				} else {
					resultLabel.SetText("Server failed to start — check the Logs view once you reach the dashboard.")
				}
			})
		}()
	})

	info := widget.NewLabel("Choose the port the inventory API should listen on. By default the server\n" +
		"speaks HTTPS and prefers IPv6 (falling back to IPv4 automatically). If this\n" +
		"machine has a public domain name pointed at it (with ports 80/443 reachable),\n" +
		"enter it below for a real, automatically renewed certificate via Let's Encrypt.\n" +
		"Leave it blank for LAN/IP-only access — a self-signed certificate will be\n" +
		"generated and reused; client devices will need to accept/trust it once.\n\n" +
		"Or check \"Localhost only\" below to restrict the server to this machine —\n" +
		"it won't be reachable over the network at all, so it uses plain HTTP with no\n" +
		"certificate (loopback traffic never leaves the machine, so there's nothing to\n" +
		"encrypt or trust).")
	info.Wrapping = fyne.TextWrapWord

	form := container.NewVBox(
		portEntry,
		localhostCheck,
		tlsForm,
		startBtn, resultLabel, copyBtn,
	)

	return container.NewVBox(
		widget.NewCard("Expose API Route", "", container.NewVBox(info, form)),
	)
}

// ---------------------------------------------------------------------------
// Finish: persist everything collected during the wizard
// ---------------------------------------------------------------------------

func finishSetup() {
	if _, err := createUser("admin", wizard.adminPassword, true); err != nil {
		dialog.ShowError(fmt.Errorf("failed to create admin account: %w", err), mainWindow)
		return
	}
	for _, u := range wizardPendingUsers {
		if _, err := createUser(u.Username, u.Password, false); err != nil {
			dialog.ShowError(fmt.Errorf("failed to create user %s: %w", u.Username, err), mainWindow)
			return
		}
	}

	if wizard.mapSrcPath != "" {
		// buildMapStep already staged the bytes to ./map.svg when the file
		// was picked; this is a best-effort re-copy in case that path is
		// still reachable and shouldn't block setup if it isn't.
		_ = copyMapFile(wizard.mapSrcPath)
	}

	for _, te := range wizard.tables {
		ensureTableGridSize(te)
		t := Table{
			ID: te.ID, Name: te.Name, GridRows: te.GridRows, GridCols: te.GridCols,
			StackType: te.StackType, StackCols: te.StackCols, StackRows: te.StackRows,
		}
		if err := createTable(t); err != nil {
			dialog.ShowError(fmt.Errorf("failed to create table %s: %w", te.ID, err), mainWindow)
			return
		}
		for r := 0; r < te.GridRows; r++ {
			for c := 0; c < te.GridCols; c++ {
				if err := saveCell(te.Cells[r][c]); err != nil {
					dialog.ShowError(fmt.Errorf("failed to save layout for table %s: %w", te.ID, err), mainWindow)
					return
				}
			}
		}
	}

	if wizard.port != "" {
		_ = setSetting("server_port", wizard.port)
	}
	_ = setSetting("setup_complete", "true")

	logAction("admin", "setup_complete", fmt.Sprintf("tables=%d users=%d", len(wizard.tables), len(wizardPendingUsers)+1))

	dialog.ShowInformation("Setup Complete", "Inventory Manager is ready to use.", mainWindow)
	showDashboard()
}

var dashboardContent *fyne.Container
var serverStatusBtn *widget.Button

func showDashboard() {
	serverStatusBtn = widget.NewButton("", toggleServer)
	refreshServerStatusBtn()

	statusArea := container.NewCenter(container.NewVBox(
		widget.NewLabelWithStyle("Server Status", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		serverStatusBtn,
	))

	dashboardContent = container.NewStack(statusArea)

	navButtons := container.NewVBox(
		widget.NewButtonWithIcon("Dashboard", theme.HomeIcon(), func() { setDashboardView(statusArea) }),
		widget.NewButtonWithIcon("Accounts", theme.AccountIcon(), func() { setDashboardView(buildAccountsView()) }),
		widget.NewButtonWithIcon("Inventory Grid", theme.GridIcon(), func() { setDashboardView(buildInventoryEditorView()) }),
		widget.NewButtonWithIcon("Tables", theme.ViewRestoreIcon(), func() { setDashboardView(buildTablesView()) }),
		widget.NewButtonWithIcon("Map", theme.FolderOpenIcon(), func() { setDashboardView(buildMapView()) }),
		widget.NewButtonWithIcon("Month Codes", theme.ViewRefreshIcon(), func() { setDashboardView(buildMonthCodesView()) }),
		widget.NewButtonWithIcon("Parties", theme.AccountIcon(), func() { setDashboardView(buildPartiesView()) }),
		widget.NewButtonWithIcon("Reels", theme.StorageIcon(), func() { setDashboardView(buildReelsView()) }),
		widget.NewButtonWithIcon("Search", theme.SearchIcon(), func() { setDashboardView(buildSearchView()) }),
		widget.NewButtonWithIcon("Server Info", theme.ComputerIcon(), func() { setDashboardView(buildServerInfoView()) }),
		widget.NewButtonWithIcon("Logs", theme.DocumentIcon(), func() { setDashboardView(buildLogsView()) }),
		widget.NewButtonWithIcon("API Logs", theme.ListIcon(), func() { setDashboardView(buildAPILogsView()) }),
		widget.NewButtonWithIcon("Reset", theme.DeleteIcon(), func() { setDashboardView(buildResetView()) }),
		layout.NewSpacer(),
		widget.NewButtonWithIcon("Hide to Tray", theme.LogoutIcon(), func() { mainWindow.Hide() }),
	)
	navScroll := container.NewVScroll(navButtons)
	navScroll.SetMinSize(fyne.NewSize(180, 0))

	title := widget.NewLabelWithStyle("Inventory Manager", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	header := container.NewHBox(title, layout.NewSpacer(), widget.NewLabel("Logged in as: admin"))

	root := container.NewBorder(header, nil, navScroll, nil, dashboardContent)
	mainWindow.SetContent(root)

	// keep the status button in sync periodically
	go func() {
		for {
			time.Sleep(2 * time.Second)
			fyne.Do(refreshServerStatusBtn)
		}
	}()
}

func setDashboardView(obj fyne.CanvasObject) {
	dashboardContent.Objects = []fyne.CanvasObject{obj}
	dashboardContent.Refresh()
}

func refreshServerStatusBtn() {
	if serverStatusBtn == nil {
		return
	}
	if isServerRunning() {
		serverStatusBtn.SetText("● RUNNING — click to stop")
		serverStatusBtn.Importance = widget.SuccessImportance
	} else {
		serverStatusBtn.SetText("● STOPPED — click to start")
		serverStatusBtn.Importance = widget.DangerImportance
	}
	serverStatusBtn.Refresh()
}

func toggleServer() {
	if isServerRunning() {
		stopServer()
	} else {
		port := getSetting("server_port")
		if port == "" {
			port = "8080"
		}
		go startServer(port)
	}
	refreshServerStatusBtn()
}

// ---------------------------------------------------------------------------
// Accounts view
// ---------------------------------------------------------------------------

func buildAccountsView() fyne.CanvasObject {
	users, _ := listUsers()

	list := widget.NewList(
		func() int { return len(users) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(widget.NewButton("Reset Password", nil), widget.NewButton("Delete", nil)),
				widget.NewLabel(""))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			btnBox := row.Objects[1].(*fyne.Container)
			resetBtn := btnBox.Objects[0].(*widget.Button)
			delBtn := btnBox.Objects[1].(*widget.Button)

			u := users[id]
			roleTag := ""
			if u.IsAdmin {
				roleTag = " (admin)"
			}
			label.SetText(fmt.Sprintf("#%d %s%s", u.ID, u.Username, roleTag))

			resetBtn.OnTapped = func() {
				promptNewPassword(u)
			}
			delBtn.OnTapped = func() {
				if u.IsAdmin {
					dialog.ShowError(fmt.Errorf("the admin account cannot be deleted"), mainWindow)
					return
				}
				dialog.ShowConfirm("Delete User", "Delete user '"+u.Username+"'?", func(ok bool) {
					if !ok {
						return
					}
					if err := deleteUser(u.ID); err != nil {
						dialog.ShowError(err, mainWindow)
						return
					}
					logAction("admin", "delete_user", u.Username)
					setDashboardView(buildAccountsView())
				}, mainWindow)
			}
			delBtn.Disable()
			if !u.IsAdmin {
				delBtn.Enable()
			}
		},
	)

	unameEntry := widget.NewEntry()
	unameEntry.SetPlaceHolder("New username")
	pwEntry := widget.NewPasswordEntry()
	pwEntry.SetPlaceHolder("New password")
	addBtn := widget.NewButtonWithIcon("Add User", theme.ContentAddIcon(), func() {
		if unameEntry.Text == "" || pwEntry.Text == "" {
			dialog.ShowError(fmt.Errorf("username and password are required"), mainWindow)
			return
		}
		if _, err := createUser(unameEntry.Text, pwEntry.Text, false); err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		logAction("admin", "create_user", unameEntry.Text)
		setDashboardView(buildAccountsView())
	})

	addForm := container.NewBorder(nil, nil, nil, addBtn, container.NewGridWithColumns(2, unameEntry, pwEntry))

	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("Accounts", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), addForm, widget.NewSeparator()),
		nil, nil, nil, list,
	)
}

func promptNewPassword(u User) {
	pw := widget.NewPasswordEntry()
	pw.SetPlaceHolder("New password")
	dialog.ShowForm("Reset Password for "+u.Username, "Save", "Cancel",
		[]*widget.FormItem{widget.NewFormItem("New Password", pw)},
		func(ok bool) {
			if !ok || pw.Text == "" {
				return
			}
			if err := updateUserPassword(u.ID, pw.Text); err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			logAction("admin", "reset_password", u.Username)
			dialog.ShowInformation("Done", "Password updated for "+u.Username, mainWindow)
		}, mainWindow)
}

// ---------------------------------------------------------------------------
// Inventory grid editor view
// ---------------------------------------------------------------------------

func buildInventoryEditorView() fyne.CanvasObject {
	tables, _ := getAllTables()
	if len(tables) == 0 {
		return container.NewVBox(
			widget.NewLabelWithStyle("Inventory Grid", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			widget.NewSeparator(),
			widget.NewLabel("No tables configured yet. Add one from the Tables view, or upload a\nmap.svg there and rescan it to discover regions."),
		)
	}

	ids := make([]string, len(tables))
	byID := map[string]Table{}
	for i, t := range tables {
		ids[i] = t.ID
		byID[t.ID] = t
	}

	var current *wizardTableEntry
	content := container.NewStack()

	loadTable := func(id string) {
		t := byID[id]
		current = &wizardTableEntry{
			ID: t.ID, Name: t.Name, GridRows: t.GridRows, GridCols: t.GridCols,
			StackType: t.StackType, StackCols: t.StackCols, StackRows: t.StackRows,
		}
		cells, _ := getAllCells(t.ID)
		current.Cells = make([][]Cell, t.GridRows)
		for r := 0; r < t.GridRows; r++ {
			current.Cells[r] = make([]Cell, t.GridCols)
		}
		for _, c := range cells {
			if c.Row < t.GridRows && c.Col < t.GridCols {
				current.Cells[c.Row][c.Col] = c
			}
		}
		ensureTableGridSize(current)
		content.Objects = []fyne.CanvasObject{buildTableGridEditor(current)}
		content.Refresh()
	}

	tableSelect := widget.NewSelect(ids, func(id string) { loadTable(id) })

	saveBtn := widget.NewButtonWithIcon("Save Grid", theme.DocumentSaveIcon(), func() {
		if current == nil {
			return
		}
		te := current
		if te.StackCols < 1 || te.StackRows < 1 {
			dialog.ShowError(fmt.Errorf("each cell's stack must have at least 1 column and 1 row"), mainWindow)
			return
		}
		if te.StackType == "horizontal" && te.StackRows > te.StackCols {
			dialog.ShowError(fmt.Errorf("stack rows (%d) cannot exceed stack columns (%d) for horizontal (tapering) stacking", te.StackRows, te.StackCols), mainWindow)
			return
		}
		for r := 0; r < te.GridRows; r++ {
			for c := 0; c < te.GridCols; c++ {
				_ = saveCell(te.Cells[r][c])
			}
		}
		_ = deleteCellsOutsideBounds(te.ID, te.GridRows, te.GridCols)
		t := Table{ID: te.ID, Name: te.Name, GridRows: te.GridRows, GridCols: te.GridCols, StackType: te.StackType, StackCols: te.StackCols, StackRows: te.StackRows}
		if err := updateTable(t); err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		logAction("admin", "grid_saved", fmt.Sprintf("table=%s rows=%d cols=%d stack_type=%s stack_cols=%d stack_rows=%d",
			te.ID, te.GridRows, te.GridCols, te.StackType, te.StackCols, te.StackRows))
		broadcastEvent("grid_saved", "admin", fiber.Map{
			"table_id": te.ID, "grid_rows": te.GridRows, "grid_cols": te.GridCols,
			"stack_type": te.StackType, "stack_cols": te.StackCols, "stack_rows": te.StackRows,
		})
		dialog.ShowInformation("Saved", "Inventory grid updated.", mainWindow)
	})

	loadTable(ids[0])
	tableSelect.SetSelected(ids[0])

	header := container.NewVBox(
		widget.NewLabelWithStyle("Inventory Grid Editor", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, widget.NewLabel("Table:"), saveBtn, tableSelect),
		widget.NewSeparator(),
	)

	return container.NewBorder(header, nil, nil, nil, content)
}

// ---------------------------------------------------------------------------
// Tables view — admin CRUD for physical tables/regions, plus a "Rescan Map"
// action that registers any new region ids found in ./map.svg without
// touching tables that are already configured.
// ---------------------------------------------------------------------------

func buildTablesView() fyne.CanvasObject {
	tables, _ := getAllTables()

	list := widget.NewList(
		func() int { return len(tables) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(widget.NewButton("Edit", nil), widget.NewButton("Delete", nil)),
				widget.NewLabel(""))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			t := tables[id]
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			btns := row.Objects[1].(*fyne.Container)
			editBtn := btns.Objects[0].(*widget.Button)
			delBtn := btns.Objects[1].(*widget.Button)
			label.SetText(fmt.Sprintf("%s — %s — %dx%d grid, %s stacking, row length %d, %d level(s)",
				t.ID, t.Name, t.GridRows, t.GridCols, t.StackType, t.StackCols, t.StackRows))
			editBtn.OnTapped = func() {
				tCopy := t
				showTableForm(&tCopy, func() { setDashboardView(buildTablesView()) })
			}
			delBtn.OnTapped = func() {
				tID := t.ID
				dialog.ShowConfirm("Delete Table", fmt.Sprintf("Delete table %q and all of its cells? This cannot be undone.", tID), func(ok bool) {
					if !ok {
						return
					}
					if err := deleteTable(tID); err != nil {
						dialog.ShowError(err, mainWindow)
						return
					}
					logAction("admin", "table_delete", "id="+tID)
					broadcastEvent("table_delete", "admin", fiber.Map{"id": tID})
					setDashboardView(buildTablesView())
				}, mainWindow)
			}
		},
	)

	addBtn := widget.NewButtonWithIcon("Add Table", theme.ContentAddIcon(), func() {
		showTableForm(nil, func() { setDashboardView(buildTablesView()) })
	})
	rescanBtn := widget.NewButtonWithIcon("Rescan Map", theme.ViewRefreshIcon(), func() {
		if !mapFileExists() {
			dialog.ShowError(fmt.Errorf("no map.svg has been uploaded yet — see the Map view"), mainWindow)
			return
		}
		ids, err := parseMapTableIDs(mapSVGFile)
		if err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		added := 0
		for _, id := range ids {
			if _, terr := getTable(id); terr != nil {
				if cerr := createTable(defaultTable(id)); cerr == nil {
					added++
				}
			}
		}
		logAction("admin", "tables_rescanned", fmt.Sprintf("found=%d added=%d", len(ids), added))
		dialog.ShowInformation("Rescan Complete", fmt.Sprintf("Found %d region(s) in map.svg; added %d new table(s).", len(ids), added), mainWindow)
		setDashboardView(buildTablesView())
	})

	return container.NewBorder(
		container.NewVBox(
			widget.NewLabelWithStyle("Tables", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			container.NewHBox(addBtn, rescanBtn),
			widget.NewSeparator(),
		),
		nil, nil, nil, container.NewVScroll(list),
	)
}

// showTableForm opens an add/edit dialog for one table's parameters.
// Pass existing=nil to create a new table.
func showTableForm(existing *Table, onDone func()) {
	idEntry := widget.NewEntry()
	idEntry.SetPlaceHolder("must match a map.svg region id")
	nameEntry := widget.NewEntry()
	rowsEntry := widget.NewEntry()
	colsEntry := widget.NewEntry()
	stackTypeSelect := widget.NewSelect([]string{"vertical", "horizontal"}, nil)
	rowLengthEntry := widget.NewEntry()
	levelsEntry := widget.NewEntry()

	if existing != nil {
		idEntry.SetText(existing.ID)
		idEntry.Disable()
		nameEntry.SetText(existing.Name)
		rowsEntry.SetText(strconv.Itoa(existing.GridRows))
		colsEntry.SetText(strconv.Itoa(existing.GridCols))
		stackTypeSelect.SetSelected(existing.StackType)
		rowLengthEntry.SetText(strconv.Itoa(existing.StackCols))
		levelsEntry.SetText(strconv.Itoa(existing.StackRows))
	} else {
		rowsEntry.SetText("1")
		colsEntry.SetText("1")
		stackTypeSelect.SetSelected("vertical")
		rowLengthEntry.SetText("3")
		levelsEntry.SetText("3")
	}

	form := widget.NewForm(
		widget.NewFormItem("ID", idEntry),
		widget.NewFormItem("Name", nameEntry),
		widget.NewFormItem("Grid rows", rowsEntry),
		widget.NewFormItem("Grid cols", colsEntry),
		widget.NewFormItem("Stacking type", stackTypeSelect),
		widget.NewFormItem("Row length (slots per stack row)", rowLengthEntry),
		widget.NewFormItem("Stack levels", levelsEntry),
	)

	title := "Add Table"
	if existing != nil {
		title = "Edit Table"
	}

	dialog.ShowCustomConfirm(title, "Save", "Cancel", form, func(ok bool) {
		if !ok {
			return
		}
		rows, _ := strconv.Atoi(rowsEntry.Text)
		cols, _ := strconv.Atoi(colsEntry.Text)
		stackCols, _ := strconv.Atoi(rowLengthEntry.Text)
		stackRows, _ := strconv.Atoi(levelsEntry.Text)
		t := Table{
			ID: idEntry.Text, Name: nameEntry.Text, GridRows: rows, GridCols: cols,
			StackType: stackTypeSelect.Selected, StackCols: stackCols, StackRows: stackRows,
		}
		var err error
		action := "table_create"
		if existing != nil {
			err = updateTable(t)
			action = "table_update"
		} else {
			err = createTable(t)
		}
		if err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		logAction("admin", action, "id="+t.ID)
		broadcastEvent(action, "admin", t)
		onDone()
	}, mainWindow)
}

// ---------------------------------------------------------------------------
// Map view — preview the uploaded map.svg and replace it with a new file
// (copies the chosen file to ./map.svg, same as the setup wizard's upload
// step).
// ---------------------------------------------------------------------------

func buildMapView() fyne.CanvasObject {
	status := widget.NewLabel("No map uploaded yet.")
	status.Wrapping = fyne.TextWrapWord
	preview := container.NewStack()

	refresh := func() {
		if !mapFileExists() {
			status.SetText("No map uploaded yet.")
			preview.Objects = nil
			preview.Refresh()
			return
		}
		ids, err := parseMapTableIDs(mapSVGFile)
		if err != nil {
			status.SetText("map.svg exists but failed to parse: " + err.Error())
		} else {
			status.SetText(fmt.Sprintf("map.svg is loaded — %d region(s): %s", len(ids), strings.Join(ids, ", ")))
		}
		if res, rerr := fyne.LoadResourceFromPath(mapSVGFile); rerr == nil {
			img := canvas.NewImageFromResource(res)
			img.FillMode = canvas.ImageFillContain
			img.SetMinSize(fyne.NewSize(480, 360))
			preview.Objects = []fyne.CanvasObject{img}
		}
		preview.Refresh()
	}
	refresh()

	changeBtn := widget.NewButtonWithIcon("Change Map…", theme.FolderOpenIcon(), func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, ferr error) {
			if ferr != nil {
				dialog.ShowError(ferr, mainWindow)
				return
			}
			if rc == nil {
				return // cancelled
			}
			defer rc.Close()
			data, rerr := io.ReadAll(rc)
			if rerr != nil {
				dialog.ShowError(rerr, mainWindow)
				return
			}
			if werr := os.WriteFile(mapSVGFile, data, 0644); werr != nil {
				dialog.ShowError(fmt.Errorf("failed to save map: %w", werr), mainWindow)
				return
			}
			logAction("admin", "map_replaced", rc.URI().Path())
			broadcastEvent("map_replaced", "admin", nil)
			dialog.ShowInformation("Map Updated", "map.svg has been replaced. Visit Tables → Rescan Map to pick up any new regions.", mainWindow)
			refresh()
		}, mainWindow)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".svg"}))
		fd.Show()
	})

	info := widget.NewLabel("Uploading a new file here copies it to map.svg next to the app,\nreplacing the current one. Existing tables are kept — use Tables →\nRescan Map afterwards to register any newly added regions.")
	info.Wrapping = fyne.TextWrapWord

	return container.NewVBox(
		widget.NewLabelWithStyle("Map", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		status, preview, changeBtn, info,
	)
}

// ---------------------------------------------------------------------------
// Month codes view — admin manages the list of month codes available to
// each cell. Items placed in that cell (via the API) are then tagged with
// one of these codes.
// ---------------------------------------------------------------------------

func buildMonthCodesView() fyne.CanvasObject {
	codes, _ := getAllMonthCodes()

	list := widget.NewList(
		func() int { return len(codes) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil, widget.NewButtonWithIcon("", theme.DeleteIcon(), nil), widget.NewLabel(""))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			delBtn := row.Objects[1].(*widget.Button)

			m := codes[id]
			label.SetText(fmt.Sprintf("#%d  %s", m.ID, m.Code))
			delBtn.OnTapped = func() {
				dialog.ShowConfirm("Delete Month Code", fmt.Sprintf("Delete month code %q?", m.Code), func(ok bool) {
					if !ok {
						return
					}
					if err := removeMonthCode(m.ID); err != nil {
						dialog.ShowError(err, mainWindow)
						return
					}
					logAction("admin", "monthcode_remove", fmt.Sprintf("id=%d", m.ID))
					broadcastEvent("monthcode_remove", "admin", fiber.Map{"id": m.ID})
					setDashboardView(buildMonthCodesView())
				}, mainWindow)
			}
		},
	)

	newCodeEntry := widget.NewEntry()
	newCodeEntry.SetPlaceHolder("e.g. 2026-07")
	addBtn := widget.NewButtonWithIcon("Add Month Code", theme.ContentAddIcon(), func() {
		if newCodeEntry.Text == "" {
			return
		}
		id, err := addMonthCode(newCodeEntry.Text)
		if err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		logAction("admin", "monthcode_add", fmt.Sprintf("id=%d code=%s", id, newCodeEntry.Text))
		broadcastEvent("monthcode_add", "admin", fiber.Map{"id": id, "code": newCodeEntry.Text})
		newCodeEntry.SetText("")
		setDashboardView(buildMonthCodesView())
	})
	addForm := container.NewBorder(nil, nil, nil, addBtn, newCodeEntry)

	info := widget.NewLabel("Month codes are a global picklist. Items added to any cell (via the\nAPI) are tagged with one of these codes.")
	info.Wrapping = fyne.TextWrapWord

	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("Month Codes", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), info, addForm, widget.NewSeparator()),
		nil, nil, nil, list,
	)
}

// ---------------------------------------------------------------------------
// Parties view — manage the global party name picklist.
// ---------------------------------------------------------------------------

func buildPartiesView() fyne.CanvasObject {
	parties, _ := getAllParties()

	list := widget.NewList(
		func() int { return len(parties) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(widget.NewButton("Edit", nil), widget.NewButton("Delete", nil)),
				widget.NewLabel(""))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			btnBox := row.Objects[1].(*fyne.Container)
			editBtn := btnBox.Objects[0].(*widget.Button)
			delBtn := btnBox.Objects[1].(*widget.Button)

			p := parties[id]
			text := fmt.Sprintf("#%d  %s", p.ID, p.Name)
			if p.Note != "" {
				text += "  (" + p.Note + ")"
			}
			label.SetText(text)

			editBtn.OnTapped = func() { showPartyForm(&p, func() { setDashboardView(buildPartiesView()) }) }
			delBtn.OnTapped = func() {
				dialog.ShowConfirm("Delete Party", fmt.Sprintf("Delete party %q?", p.Name), func(ok bool) {
					if !ok {
						return
					}
					if err := removeParty(p.ID); err != nil {
						dialog.ShowError(err, mainWindow)
						return
					}
					logAction("admin", "party_remove", fmt.Sprintf("id=%d", p.ID))
					broadcastEvent("party_remove", "admin", fiber.Map{"id": p.ID})
					setDashboardView(buildPartiesView())
				}, mainWindow)
			}
		},
	)

	addBtn := widget.NewButtonWithIcon("Add Party", theme.ContentAddIcon(), func() {
		showPartyForm(nil, func() { setDashboardView(buildPartiesView()) })
	})

	info := widget.NewLabel("Parties are a global picklist. Each item placed in a cell (via the\nAPI) can be tagged with one of these party names.")
	info.Wrapping = fyne.TextWrapWord

	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("Parties", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), info, addBtn, widget.NewSeparator()),
		nil, nil, nil, list,
	)
}

// showPartyForm opens an add/edit dialog for one party. Pass an existing
// *Party to edit it, or nil to create a new one.
func showPartyForm(existing *Party, onDone func()) {
	p := Party{}
	if existing != nil {
		p = *existing
	}

	nameEntry := widget.NewEntry()
	nameEntry.SetText(p.Name)
	noteEntry := widget.NewEntry()
	noteEntry.SetPlaceHolder("optional note")
	noteEntry.SetText(p.Note)

	title := "Add Party"
	if existing != nil {
		title = fmt.Sprintf("Edit Party #%d", existing.ID)
	}

	dialog.ShowForm(title, "Save", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Name", nameEntry),
		widget.NewFormItem("Note", noteEntry),
	}, func(ok bool) {
		if !ok {
			return
		}
		if nameEntry.Text == "" {
			dialog.ShowError(fmt.Errorf("party name is required"), mainWindow)
			return
		}
		p.Name = nameEntry.Text
		p.Note = noteEntry.Text

		if existing == nil {
			id, err := addParty(p)
			if err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			p.ID = int(id)
			logAction("admin", "party_add", fmt.Sprintf("id=%d name=%s", id, p.Name))
			broadcastEvent("party_add", "admin", p)
		} else {
			if err := modifyParty(p); err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			logAction("admin", "party_modify", fmt.Sprintf("id=%d", p.ID))
			broadcastEvent("party_modify", "admin", p)
		}
		onDone()
	}, mainWindow)
}

// showQRCodeDialog renders `text` as a level-H QR code and shows it in a
// dialog — used from the Reels view to generate a scannable code for a
// reel ID, but works for any text.
func showQRCodeDialog(text string) {
	if text == "" {
		dialog.ShowError(fmt.Errorf("nothing to encode — this reel has no reel ID set"), mainWindow)
		return
	}
	png, err := qrcode.Encode(text, qrcode.Highest, 256)
	if err != nil {
		dialog.ShowError(fmt.Errorf("failed to generate QR code: %w", err), mainWindow)
		return
	}
	img := canvas.NewImageFromResource(fyne.NewStaticResource("qrcode.png", png))
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(256, 256))

	label := widget.NewLabel(text)
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter

	dialog.ShowCustom("QR Code", "Close", container.NewVBox(img, label), mainWindow)
}

func buildReelsView() fyne.CanvasObject {
	reels, _ := allReels()

	list := widget.NewList(
		func() int { return len(reels) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(widget.NewButton("QR", nil), widget.NewButton("Edit", nil), widget.NewButton("Delete", nil)),
				widget.NewLabel(""))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			btnBox := row.Objects[1].(*fyne.Container)
			qrBtn := btnBox.Objects[0].(*widget.Button)
			editBtn := btnBox.Objects[1].(*widget.Button)
			delBtn := btnBox.Objects[2].(*widget.Button)

			r := reels[id]
			label.SetText(fmt.Sprintf("#%d  reel=%s  month=%s  item=%d  colour=%s  quality=%s  party=%s",
				r.ID, r.ReelID, r.MonthCode, r.ItemNumber, r.Colour, r.Quality, r.Party))

			qrBtn.OnTapped = func() { showQRCodeDialog(r.ReelID) }
			editBtn.OnTapped = func() { showReelForm(&r, func() { setDashboardView(buildReelsView()) }) }
			delBtn.OnTapped = func() {
				dialog.ShowConfirm("Delete Reel", fmt.Sprintf("Delete reel record #%d (%s)?", r.ID, r.ReelID), func(ok bool) {
					if !ok {
						return
					}
					if err := removeReel(r.ID); err != nil {
						dialog.ShowError(err, mainWindow)
						return
					}
					logAction("admin", "reel_remove", fmt.Sprintf("id=%d", r.ID))
					broadcastEvent("reel_remove", "admin", fiber.Map{"id": r.ID})
					setDashboardView(buildReelsView())
				}, mainWindow)
			}
		},
	)

	addBtn := widget.NewButtonWithIcon("Add Reel Record", theme.ContentAddIcon(), func() {
		showReelForm(nil, func() { setDashboardView(buildReelsView()) })
	})

	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("Reel Records", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), addBtn, widget.NewSeparator()),
		nil, nil, nil, list,
	)
}

// showReelForm opens an add/edit dialog for one reel record. Pass an
// existing *Reel to edit it, or nil to create a new one.
func showReelForm(existing *Reel, onDone func()) {
	r := Reel{}
	if existing != nil {
		r = *existing
	}

	reelIDEntry := widget.NewEntry()
	reelIDEntry.SetText(r.ReelID)
	monthCodeEntry := widget.NewEntry()
	monthCodeEntry.SetText(r.MonthCode)
	itemNumberEntry := widget.NewEntry()
	if r.ItemNumber != 0 {
		itemNumberEntry.SetText(strconv.Itoa(r.ItemNumber))
	}
	sizeEntry := widget.NewEntry()
	sizeEntry.SetPlaceHolder("cm")
	if r.SizeCM != 0 {
		sizeEntry.SetText(strconv.FormatFloat(r.SizeCM, 'f', -1, 64))
	}
	gsmEntry := widget.NewEntry()
	if r.GSM != 0 {
		gsmEntry.SetText(strconv.FormatFloat(r.GSM, 'f', -1, 64))
	}
	bfEntry := widget.NewEntry()
	bfEntry.SetText(r.BF)
	colourEntry := widget.NewEntry()
	colourEntry.SetText(r.Colour)
	weightEntry := widget.NewEntry()
	weightEntry.SetPlaceHolder("kg")
	if r.WeightKg != 0 {
		weightEntry.SetText(strconv.FormatFloat(r.WeightKg, 'f', -1, 64))
	}
	dateEntry := widget.NewEntry()
	dateEntry.SetPlaceHolder("YYYY-MM-DD")
	dateEntry.SetText(r.Date)
	timeEntry := widget.NewEntry()
	timeEntry.SetPlaceHolder("HH:MM:SS")
	timeEntry.SetText(r.Time)
	qualityEntry := widget.NewEntry()
	qualityEntry.SetText(r.Quality)

	parties, _ := getAllParties()
	partyNames := make([]string, 0, len(parties)+1)
	for _, p := range parties {
		partyNames = append(partyNames, p.Name)
	}
	if r.Party != "" {
		found := false
		for _, n := range partyNames {
			if n == r.Party {
				found = true
				break
			}
		}
		if !found {
			partyNames = append(partyNames, r.Party)
		}
	}
	partySelect := widget.NewSelect(partyNames, nil)
	if r.Party != "" {
		partySelect.SetSelected(r.Party)
	}
	var partyField fyne.CanvasObject = partySelect
	if len(partyNames) == 0 {
		partyHint := widget.NewLabel("No parties defined yet — add one from the Parties view first.")
		partyHint.Wrapping = fyne.TextWrapWord
		partyField = partyHint
	}

	title := "Add Reel Record"
	if existing != nil {
		title = fmt.Sprintf("Edit Reel Record #%d", existing.ID)
	}

	dialog.ShowForm(title, "Save", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Reel ID", reelIDEntry),
		widget.NewFormItem("Month Code", monthCodeEntry),
		widget.NewFormItem("Item Number", itemNumberEntry),
		widget.NewFormItem("Size (cm)", sizeEntry),
		widget.NewFormItem("GSM", gsmEntry),
		widget.NewFormItem("BF", bfEntry),
		widget.NewFormItem("Colour", colourEntry),
		widget.NewFormItem("Weight (kg)", weightEntry),
		widget.NewFormItem("Date", dateEntry),
		widget.NewFormItem("Time (packing)", timeEntry),
		widget.NewFormItem("Quality", qualityEntry),
		widget.NewFormItem("Party", partyField),
	}, func(ok bool) {
		if !ok {
			return
		}
		if reelIDEntry.Text == "" {
			dialog.ShowError(fmt.Errorf("reel ID is required"), mainWindow)
			return
		}
		r.ReelID = reelIDEntry.Text
		r.MonthCode = monthCodeEntry.Text
		r.ItemNumber, _ = strconv.Atoi(itemNumberEntry.Text)
		r.SizeCM, _ = strconv.ParseFloat(sizeEntry.Text, 64)
		r.GSM, _ = strconv.ParseFloat(gsmEntry.Text, 64)
		r.BF = bfEntry.Text
		r.Colour = colourEntry.Text
		r.WeightKg, _ = strconv.ParseFloat(weightEntry.Text, 64)
		r.Date = dateEntry.Text
		r.Time = timeEntry.Text
		r.Quality = qualityEntry.Text
		r.Party = partySelect.Selected

		if existing == nil {
			id, err := addReel(r)
			if err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			r.ID = int(id)
			logAction("admin", "reel_add", fmt.Sprintf("id=%d reel_id=%s", id, r.ReelID))
			broadcastEvent("reel_add", "admin", r)
		} else {
			if err := modifyReel(r); err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			logAction("admin", "reel_modify", fmt.Sprintf("id=%d", r.ID))
			broadcastEvent("reel_modify", "admin", r)
		}
		onDone()
	}, mainWindow)
}

// ---------------------------------------------------------------------------
// Search view — a desktop front-end for the same modular search engine the
// /api/search endpoint uses. Filters are built programmatically as rows and
// run directly against the registry (no HTTP round-trip needed locally).
// ---------------------------------------------------------------------------

type searchFilterRow struct {
	field *widget.Entry
	op    *widget.Select
	value *widget.Entry
	box   *fyne.Container
}

func buildSearchView() fyne.CanvasObject {
	targetNames := make([]string, 0, len(searchRegistry))
	for name := range searchRegistry {
		targetNames = append(targetNames, name)
	}
	sort.Strings(targetNames)

	targetSelect := widget.NewSelect(targetNames, nil)
	targetSelect.SetSelected(targetNames[0])
	matchSelect := widget.NewSelect([]string{"all", "any"}, nil)
	matchSelect.SetSelected("all")
	sortFieldEntry := widget.NewEntry()
	sortFieldEntry.SetPlaceHolder("sort field (optional)")
	sortOrderSelect := widget.NewSelect([]string{"asc", "desc"}, nil)
	sortOrderSelect.SetSelected("asc")
	limitEntry := widget.NewEntry()
	limitEntry.SetText("100")

	fieldsHint := widget.NewLabel("")
	fieldsHint.Wrapping = fyne.TextWrapWord
	updateFieldsHint := func() {
		if t, ok := searchRegistry[targetSelect.Selected]; ok {
			fieldsHint.SetText("Fields: " + strings.Join(t.Fields, ", "))
		}
	}
	updateFieldsHint()
	targetSelect.OnChanged = func(string) { updateFieldsHint() }

	filterRows := container.NewVBox()
	var rows []*searchFilterRow

	var addFilterRow func()
	addFilterRow = func() {
		fieldEntry := widget.NewEntry()
		fieldEntry.SetPlaceHolder("field")
		opSelect := widget.NewSelect([]string{"eq", "neq", "contains", "starts_with", "ends_with", "gt", "gte", "lt", "lte", "in"}, nil)
		opSelect.SetSelected("eq")
		valueEntry := widget.NewEntry()
		valueEntry.SetPlaceHolder("value (comma-separated for 'in')")

		row := &searchFilterRow{field: fieldEntry, op: opSelect, value: valueEntry}
		removeBtn := widget.NewButtonWithIcon("", theme.ContentRemoveIcon(), func() {
			for i, r := range rows {
				if r == row {
					rows = append(rows[:i], rows[i+1:]...)
					break
				}
			}
			filterRows.Remove(row.box)
			filterRows.Refresh()
		})
		row.box = container.NewGridWithColumns(4, fieldEntry, opSelect, valueEntry, removeBtn)
		rows = append(rows, row)
		filterRows.Add(row.box)
		filterRows.Refresh()
	}
	addFilterRow() // start with one empty row

	addRowBtn := widget.NewButtonWithIcon("Add Filter", theme.ContentAddIcon(), addFilterRow)

	resultsEntry := widget.NewMultiLineEntry()
	resultsEntry.Wrapping = fyne.TextWrapOff
	resultsEntry.Disable()

	runSearch := func() {
		target, ok := searchRegistry[targetSelect.Selected]
		if !ok {
			dialog.ShowError(fmt.Errorf("unknown target %q", targetSelect.Selected), mainWindow)
			return
		}
		var filters []SearchFilter
		for _, r := range rows {
			if r.field.Text == "" || r.value.Text == "" {
				continue
			}
			var val interface{} = r.value.Text
			if r.op.Selected == "in" {
				parts := strings.Split(r.value.Text, ",")
				list := make([]interface{}, len(parts))
				for i, p := range parts {
					list[i] = strings.TrimSpace(p)
				}
				val = list
			} else if f, err := strconv.ParseFloat(r.value.Text, 64); err == nil {
				val = f // numeric-looking values are compared numerically
			}
			filters = append(filters, SearchFilter{Field: r.field.Text, Op: r.op.Selected, Value: val})
		}

		records, err := target.Fetch()
		if err != nil {
			dialog.ShowError(err, mainWindow)
			return
		}
		filtered := applyFilters(records, filters, matchSelect.Selected)
		if sortFieldEntry.Text != "" {
			sortRecords(filtered, sortFieldEntry.Text, sortOrderSelect.Selected)
		}
		limit, _ := strconv.Atoi(limitEntry.Text)
		if limit <= 0 {
			limit = 100
		}
		page := paginate(filtered, limit, 0)

		logAction("admin", "search", fmt.Sprintf("target=%s filters=%d match=%s results=%d/%d",
			targetSelect.Selected, len(filters), matchSelect.Selected, len(page), len(filtered)))

		b, _ := json.MarshalIndent(SearchResponse{Target: targetSelect.Selected, Total: len(filtered), Count: len(page), Results: page}, "", "  ")
		resultsEntry.SetText(string(b))
	}

	searchBtn := widget.NewButtonWithIcon("Search", theme.SearchIcon(), runSearch)

	controls := container.NewVBox(
		container.NewGridWithColumns(2,
			container.NewVBox(widget.NewLabel("Target"), targetSelect),
			container.NewVBox(widget.NewLabel("Match"), matchSelect),
		),
		fieldsHint,
		widget.NewSeparator(),
		filterRows,
		addRowBtn,
		widget.NewSeparator(),
		container.NewGridWithColumns(3,
			container.NewVBox(widget.NewLabel("Sort Field"), sortFieldEntry),
			container.NewVBox(widget.NewLabel("Sort Order"), sortOrderSelect),
			container.NewVBox(widget.NewLabel("Limit"), limitEntry),
		),
		searchBtn,
	)

	resultsScroll := container.NewVScroll(resultsEntry)

	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("Search", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), widget.NewSeparator(), controls, widget.NewSeparator()),
		nil, nil, nil, resultsScroll,
	)
}

// ---------------------------------------------------------------------------
// Server info view
// ---------------------------------------------------------------------------

func buildServerInfoView() fyne.CanvasObject {
	status := "STOPPED"
	if isServerRunning() {
		status = "RUNNING"
	}
	port := getSetting("server_port")
	domain := getSetting("tls_domain")
	localhostMode := getSetting("localhost_mode") == "true"

	scheme := serverScheme
	if scheme == "" {
		scheme = "https"
	}
	displayAddr := serverAddr
	if displayAddr == "" {
		if localhostMode {
			displayAddr = fmt.Sprintf("localhost:%s", port)
			scheme = "http"
		} else {
			host, isV6 := localIP()
			if isV6 {
				host = "[" + host + "]"
			}
			displayAddr = fmt.Sprintf("%s:%s", host, port)
		}
	}
	fullURL := scheme + "://" + displayAddr

	addrLabel := widget.NewLabel(fullURL)
	copyBtn := widget.NewButtonWithIcon("Copy", theme.ContentCopyIcon(), func() {
		mainWindow.Clipboard().SetContent(fullURL)
	})

	modeLabel := widget.NewLabel("self-signed (LAN/IP) — clients must trust it once")
	if localhostMode {
		modeLabel.SetText("localhost-only, plain HTTP — not reachable from the network")
	} else if domain != "" {
		modeLabel.SetText("automatic via certmagic/Let's Encrypt (domain=" + domain + ")")
	}

	fingerprintLabel := widget.NewLabel("")
	fingerprintLabel.Wrapping = fyne.TextWrapWord
	if localhostMode {
		fingerprintLabel.SetText("No certificate needed — loopback-only traffic never leaves this machine.")
	} else if fp, err := selfSignedCertFingerprint(); err == nil {
		fingerprintLabel.SetText("Self-signed cert SHA-256: " + fp)
	} else {
		fingerprintLabel.SetText("No self-signed certificate generated yet (starts on first HTTPS launch without a domain).")
	}

	ip6 := localIPv6()
	if ip6 == "" {
		ip6 = "(none detected)"
	}

	info := widget.NewForm(
		widget.NewFormItem("Status", widget.NewLabel(status)),
		widget.NewFormItem("Local IPv6", widget.NewLabel(ip6)),
		widget.NewFormItem("Local IPv4", widget.NewLabel(localIPv4())),
		widget.NewFormItem("Full Address", container.NewHBox(addrLabel, copyBtn)),
		widget.NewFormItem("Mode", modeLabel),
	)

	portEntry := widget.NewEntry()
	portEntry.SetText(port)
	portHint := widget.NewLabel("Ports below 1024 (e.g. 80, 443) need Administrator (Windows) or root\n(Linux) — you'll be prompted to relaunch elevated if needed.")
	portHint.Wrapping = fyne.TextWrapWord

	domainEntry := widget.NewEntry()
	domainEntry.SetText(domain)
	domainEntry.SetPlaceHolder("leave blank for self-signed LAN/IP access")
	emailEntry := widget.NewEntry()
	emailEntry.SetText(getSetting("tls_email"))
	emailEntry.SetPlaceHolder("optional — for Let's Encrypt renewal notices")
	tlsForm := widget.NewForm(
		widget.NewFormItem("TLS Domain", domainEntry),
		widget.NewFormItem("Contact Email", emailEntry),
	)

	localhostCheck := widget.NewCheck("Localhost only — don't expose a network port, no certificate needed", nil)
	localhostCheck.SetChecked(localhostMode)
	localhostCheck.OnChanged = func(checked bool) {
		if checked {
			tlsForm.Hide()
		} else {
			tlsForm.Show()
		}
	}
	if localhostMode {
		tlsForm.Hide()
	}

	applyPortBtn := widget.NewButtonWithIcon("Apply Port", theme.DocumentSaveIcon(), func() {
		newPort := strings.TrimSpace(portEntry.Text)
		n, err := strconv.Atoi(newPort)
		if err != nil || n < 1 || n > 65535 {
			dialog.ShowError(fmt.Errorf("port must be a number between 1 and 65535"), mainWindow)
			return
		}
		_ = setSetting("server_port", newPort)
		logAction("admin", "port_changed", newPort)
		if isServerRunning() {
			stopServer()
		}
		go startServer(newPort)
		dialog.ShowInformation("Port Updated", fmt.Sprintf("Applying port %s — check Server Status on the dashboard.\nIf this port needs elevated privileges, a prompt will appear.", newPort), mainWindow)
		setDashboardView(buildServerInfoView())
	})
	portForm := container.NewBorder(nil, nil, nil, applyPortBtn, portEntry)

	saveBtn := widget.NewButtonWithIcon("Save & Restart Server", theme.DocumentSaveIcon(), func() {
		_ = setSetting("tls_domain", domainEntry.Text)
		_ = setSetting("tls_email", emailEntry.Text)
		if localhostCheck.Checked {
			_ = setSetting("localhost_mode", "true")
		} else {
			_ = setSetting("localhost_mode", "false")
		}
		logAction("admin", "server_mode_saved", fmt.Sprintf("localhost_mode=%v domain=%s", localhostCheck.Checked, domainEntry.Text))
		if isServerRunning() {
			stopServer()
		}
		go startServer(getSetting("server_port"))
		dialog.ShowInformation("Saved", "Settings saved and the server is restarting.", mainWindow)
		setDashboardView(buildServerInfoView())
	})

	return container.NewVBox(
		widget.NewLabelWithStyle("Server Info", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		info,
		fingerprintLabel,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Port", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		portForm,
		portHint,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Access Mode", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		localhostCheck,
		tlsForm,
		saveBtn,
	)
}

// selfSignedCertFingerprint returns the SHA-256 fingerprint of the cached
// self-signed certificate (if one has been generated), formatted as hex
// pairs, so an admin can read it aloud to verify a client is trusting the
// right certificate.
func selfSignedCertFingerprint() (string, error) {
	certPEM, err := os.ReadFile(selfSignedCertPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("invalid certificate file")
	}
	sum := sha256.Sum256(block.Bytes)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":"), nil
}

// ---------------------------------------------------------------------------
// Logs view (logs.txt)
// ---------------------------------------------------------------------------

func buildLogsView() fyne.CanvasObject {
	entry := widget.NewMultiLineEntry()
	entry.SetText(readLogFile())
	entry.Disable()
	entry.Wrapping = fyne.TextWrapWord

	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() {
		entry.SetText(readLogFile())
	})

	scroll := container.NewVScroll(entry)
	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("Server Logs (logs.txt, last 1024 lines)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), refreshBtn, widget.NewSeparator()),
		nil, nil, nil, scroll,
	)
}

// ---------------------------------------------------------------------------
// API logs view (DB-backed logs table)
// ---------------------------------------------------------------------------

func buildAPILogsView() fyne.CanvasObject {
	entry := widget.NewMultiLineEntry()
	entry.SetText(readAPILogsFromDB())
	entry.Disable()
	entry.Wrapping = fyne.TextWrapWord

	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() {
		entry.SetText(readAPILogsFromDB())
	})

	scroll := container.NewVScroll(entry)
	return container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("API Access Logs (last 1024 entries)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), refreshBtn, widget.NewSeparator()),
		nil, nil, nil, scroll,
	)
}

// ---------------------------------------------------------------------------
// Reset view
// ---------------------------------------------------------------------------

func buildResetView() fyne.CanvasObject {
	warn := canvas.NewText("This clears every item from every cell in the inventory grid. Cell\ncolours and numbers are kept. This cannot be undone.", theme.ErrorColor())
	warn.TextStyle = fyne.TextStyle{Bold: true}

	resetBtn := widget.NewButtonWithIcon("Reset All Items", theme.DeleteIcon(), func() {
		dialog.ShowConfirm("Confirm Reset", "Really clear all items from the inventory grid?", func(ok bool) {
			if !ok {
				return
			}
			if err := clearAllItems(); err != nil {
				dialog.ShowError(err, mainWindow)
				return
			}
			logAction("admin", "reset_items", "all cells cleared")
			broadcastEvent("reset", "admin", nil)
			dialog.ShowInformation("Done", "All items have been cleared.", mainWindow)
		}, mainWindow)
	})
	resetBtn.Importance = widget.DangerImportance

	return container.NewVBox(widget.NewLabelWithStyle("Reset Inventory", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), widget.NewSeparator(), warn, resetBtn)
}
