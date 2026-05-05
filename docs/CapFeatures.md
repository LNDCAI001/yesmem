# Capability Memory — Feature Documentation

Zusammenfassung aller Capabilities-Features: Design, Architektur, Implementierungsstand, offene Punkte. Konsolidiert aus Spec, Plänen, Git-History, Code-Audit und Session-Learnings. Verifiziert gegen Code-Stand `feat+capability-memory` 2026-04-21.

---

## 1. Problemstellung

Claude Code verliert zwischen Sessions **erlerntes Werkzeugwissen**: REPL-Snippets, API-Wrapper, Analyse-Pipelines, Daten-Fetcher. Jede neue Session startet bei Null — selbst wenn identische Werkzeuge in vorherigen Sessions gebaut und getestet wurden.

**Ziel:** Claude soll sich an selbstgebaute Werkzeuge erinnern, wissen was existiert, und sie on-demand aktivieren — ohne Token-Bloat durch dauerhaft geladene Schemata.

---

## 2. Drei-Ebenen-Architektur

```
┌───────────────────────────────────────────────────────────────────┐
│ EBENE A — Katalog im Briefing (Session-Start, einmalig)           │
│   Name + Einzeiler pro Capability. Keine Schemata.                │
│   ~80 Tokens/Cap → 100 Caps ≈ 8k Tokens (gecached).              │
│   renderCapabilitiesCatalog() in caps_inject.go.                  │
│   Format: <capabilities-available> Block als system-reminder.      │
│   Enthält Bootstrapper: registerTool("activate_cap",...)    │
│                                                                     │
│ EBENE B — Deliberative Aktivierung (pro Session, on-demand)        │
│   Bootstrapper-REPL-Tool "activate_cap" ruft intern         │
│   MCP-Tool "activate_cap" auf.                                     │
│     1. Lädt Cap aus DB (Learnings mit category="cap")              │
│     2. Gibt generierten registerTool()-Code zurück                 │
│     3. Trägt Cap in session_active_caps ein                        │
│   ⚠ Naming-Diskrepanz: Bootstrapper referenziert                  │
│     mcp__yesmem__activate_cap, MCP registriert              │
│     activate_cap. Funktioniert nur wenn Alias existiert.           │
│                                                                     │
│ EBENE C — Proxy Schema-Injection (ab Turn N+1)                     │
│   Proxy liest active caps der Session via capsCache,               │
│   ruft injectCapabilitiesTurn() auf,                               │
│   hängt deren Schemata an req["tools"] an.                         │
│   Tool wird nativer API-Call — kein REPL-Umweg mehr.               │
└───────────────────────────────────────────────────────────────────┘
```

**Warum deliberativ statt embedding-ranked:** Auto-Injection per Cosine-Match wurde evaluiert und verworfen. Das Modell selbst wählt zuverlässiger als ein heuristischer Ranker, sobald das Directory sichtbar ist. Kein False-Positive-Risiko, keine Vektor-Ingest-Pipeline nötig.

---

## 3. Datenmodell

### Capabilities als Learnings

Capabilities sind **keine eigene Tabelle**, sondern Learnings mit `category = "cap"`. Nutzt bestehende Supersede/Search/Embed-Infrastruktur.

```
learnings (yesmem.db)
├── id, content, category="cap", source, project
├── context → JSON: CapMeta (name, description, schema, handler_repl, handler_bash, tags, version, tested, auto_active)
├── keywords → Tags für Filterung
└── superseded_by → Auto-Supersede bei gleichem Cap-Name
```

### CapMeta Struct (`internal/daemon/cap_meta.go`)

```go
type CapMeta struct {
    Name        string   `json:"cap_name"`
    Description string   `json:"cap_description,omitempty"`
    Schema      string   `json:"cap_schema,omitempty"`
    HandlerREPL string   `json:"cap_handler_repl,omitempty"`
    HandlerBash string   `json:"cap_handler_bash,omitempty"`
    Tags        []string `json:"cap_tags,omitempty"`
    Version     int      `json:"cap_version"`
    Tested      bool     `json:"cap_tested"`
    TestDate    string   `json:"cap_test_date,omitempty"`
    AutoActive  bool     `json:"cap_auto_active,omitempty"`
}
```

Funktionen:

| Funktion | Signatur |
|----------|----------|
| `ParseCapMeta` | `func ParseCapMeta(s string) (CapMeta, error)` |
| `ToJSON` | `func (m CapMeta) ToJSON() (string, error)` |
| `HasTag` | `func (m CapMeta) HasTag(tag string) bool` |

### Session-Aktivierungs-State

```sql
CREATE TABLE session_active_caps (
    thread_id    TEXT    NOT NULL,
    cap_name     TEXT    NOT NULL,
    activated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    PRIMARY KEY (thread_id, cap_name)
);
```

Gespeichert in `yesmem.db`. Proxy liest per Thread-ID welche Caps aktiv sind. `auto_active` ist **kein Spalten-Attribut** hier, sondern ein Feld in `CapMeta` (Learning-Context JSON). **Default: true** — neue Caps sind standardmässig sofort für alle Sessions verfügbar.

### Cap Store (generischer KV-Store)

Separate Datenbank `capabilities.db`. Jede Capability bekommt eigene namensgebundene Tabellen:

```
capabilities.db
├── cap_store_meta               — Registry: welche Cap besitzt welche Tabellen
├── cap_{name}__{table}          — Daten-Tabellen (z.B. cap_reddit_search__listings)
└── cap_{name}__blobs            — Blob-Chunks für >30KB Payloads (geplant)
    └── PRIMARY KEY (key, chunk_idx)
```

**Quotas:**
- Max 10 Tabellen pro Capability
- Max 10.000 Rows pro Tabelle
- Max 64KB pro Zelle
- Blob-Chunking: 60KB Chunks (geplant)

**Sandboxing:** Nur `CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE`, `SELECT`. Kein `DROP`, `ALTER`, `ATTACH`, `PRAGMA`. Alle Werte via `?`-Placeholders, Namen via Regex-Whitelist (`^[a-z][a-z0-9_]{0,63}$`).

---

## 4. Komponenten-Map

### Proxy (Injection + Katalog)

**Datei: `internal/proxy/caps_inject.go`**

| Symbol | Art | Beschreibung |
|--------|-----|-------------|
| `CapInjection` | struct | Felder: `Name`, `Description`, `Schema`, `HandlerBash`, `HandlerREPL` |
| `renderCapabilitiesCatalog` | func | `func(caps []CapInjection) string` — erzeugt `<capabilities-available>` Block mit Bootstrapper-Code. Ersetzt den älteren `renderCapabilitiesBlock`. |
| `renderCapabilitiesBlock` | func | `func(caps []CapInjection) string` — ältere Variante, listet Caps als `<capabilities-active>` Block. |
| `injectCapabilitiesTurnImpl` | func | `func(req map[string]any, threadID string, queryFn ..., capsCache *CapsCache, logger *log.Logger) bool` — hängt aktive Cap-Schemata an `req["tools"]`. Dedup: native Tools gewinnen bei Name-Kollision. |
| `injectCapabilitiesTurn` | method | `func (s *Server) injectCapabilitiesTurn(req map[string]any, threadID string) bool` — Server-Method-Wrapper um `injectCapabilitiesTurnImpl`. |
| `decodeCapsResponse` | func | `func(raw json.RawMessage) ([]CapInjection, error)` — unmarshalt Daemon-Response in `[]CapInjection`. |
| `renderRegisterTool` | func | `func(c CapInjection) string` — generiert einzelnes `registerTool()`-JS-Snippet für eine Cap. |

**Datei: `internal/proxy/caps_cache.go`**

| Symbol | Art | Beschreibung |
|--------|-----|-------------|
| `CapsCache` | struct | Thread-keyed Cache. Felder: `mu sync.RWMutex`, `entries map[string][]byte`. |
| `NewCapsCache` | func | `func NewCapsCache() *CapsCache` — Konstruktor. |
| `Get` | method | `func (c *CapsCache) Get(threadID string) ([]byte, bool)` — liest Cache-Eintrag. |
| `Set` | method | `func (c *CapsCache) Set(threadID string, data []byte)` — schreibt Cache-Eintrag (Kopie). |
| `Invalidate` | method | `func (c *CapsCache) Invalidate(threadID string)` — löscht Cache-Eintrag. |
| `invalidateThreadCaches` | method | `func (s *Server) invalidateThreadCaches(threadID, project, projectDir string)` — invalidiert frozenStubs, capsCache UND briefingCache für einen Thread. |
| `cachedQueryFn` | func | `func cachedQueryFn(cache *CapsCache, threadID string, upstream func(...) ...) func(...)` — Wraps upstream-Daemon-Query, cached `get_active_caps` Responses. |

**Datei: `internal/proxy/proxy_briefing.go`**

Keine Capability-Funktionen. `renderCapabilitiesCatalog()` ist in `caps_inject.go`, nicht hier.

**Datei: `internal/proxy/proxy.go`**

Orchestrierung: ruft `capsCache` + `injectCapabilitiesTurn()` in der Request-Pipeline. Konfiguriert `cachedQueryFn()` als Query-Upstream.

### Daemon (Handler + Meta)

**Datei: `internal/daemon/cap_meta.go`**

| Funktion | Signatur |
|----------|----------|
| `ParseCapMeta` | `func ParseCapMeta(s string) (CapMeta, error)` |
| `ToJSON` | `func (m CapMeta) ToJSON() (string, error)` |
| `HasTag` | `func (m CapMeta) HasTag(tag string) bool` |

**Datei: `internal/daemon/handler_caps.go`**

| Funktion | Beschreibung |
|----------|-------------|
| `handleGetCaps` | `func (h *Handler) handleGetCaps(params map[string]any) (any, error)` — listet alle Capabilities (Learnings mit category="cap"). Filter: `name`, `tag`, `project`. |
| `handleSaveCap` | `func (h *Handler) handleSaveCap(params map[string]any) (any, error)` — speichert/updated Capability. Auto-Supersede bei gleichem Namen. `auto_active` default: **true**. |
| `handleRegisterCaps` | `func (h *Handler) handleRegisterCaps(params map[string]any) (any, error)` — Batch-Hydration: generiert `registerTool()`-JS für mehrere Caps. |
| `handleActivateCap` | `func (h *Handler) handleActivateCap(params map[string]any) (any, error)` — aktiviert einzelne Cap für Thread, gibt Code zurück. |
| `handleDeactivateCap` | `func (h *Handler) handleDeactivateCap(params map[string]any) (any, error)` — deaktiviert Cap für Thread. |
| `handleGetActiveCaps` | `func (h *Handler) handleGetActiveCaps(params map[string]any) (any, error)` — gibt aktive Caps eines Threads zurück (intern, für Proxy-Query). |

Hilfstyp: `capResult` struct (intern, für JSON-Serialisierung der Antworten).

**Datei: `internal/daemon/handler_cap_store.go`**

| Funktion | Beschreibung |
|----------|-------------|
| `handleCapStore` | `func (h *Handler) handleCapStore(params map[string]any) (any, error)` — Dispatch für Cap-Store-Actions. |

Interne Hilfsfunktionen: `capStoreCreateTable`, `capStoreUpsert`, `capStoreQuery`, `capStoreDelete`, `capStoreListTables`, `parseColumnDefs`, `parseMapParam`.

**Datei: `internal/daemon/handler.go`**

Dispatch (Zeilen ~250-260):

```go
case "get_caps":        return h.handleGetCaps(...)
case "save_cap":        return h.handleSaveCap(...)
case "register_caps":   return h.handleRegisterCaps(...)
case "activate_cap":    return h.handleActivateCap(...)
case "deactivate_cap":  return h.handleDeactivateCap(...)
case "get_active_caps": return h.handleGetActiveCaps(...)
case "cap_store":       return h.handleCapStore(...)
```

### Storage

**Datei: `internal/storage/session_caps.go`**

| Funktion | Signatur |
|----------|----------|
| `ActivateCap` | `func (s *Store) ActivateCap(threadID, name string) error` |
| `DeactivateCap` | `func (s *Store) DeactivateCap(threadID, name string) error` |
| `GetSessionCaps` | `func (s *Store) GetSessionCaps(threadID string) ([]SessionCap, error)` |
| `TouchCap` | `func (s *Store) TouchCap(threadID, name string) error` — updated `last_used_at`. |

Rückgabetyp: `SessionCap` struct (Thread-ID + Name + Timestamps).

**Datei: `internal/storage/cap_store.go`**

| Funktion | Signatur |
|----------|----------|
| `OpenCapsDB` | `func (s *Store) OpenCapsDB() error` — öffnet separate `capabilities.db`. |
| `CloseCapsDB` | `func (s *Store) CloseCapsDB()` — schliesst `capabilities.db`. |
| `CapsReady` | `func (s *Store) CapsReady() bool` — prüft ob capabilities.db geöffnet. |
| `ValidateCapName` | `func ValidateCapName(name string) error` — Regex-Validierung `^[a-z][a-z0-9_]{0,63}$`. |
| `CapsCreateTable` | `func (s *Store) CapsCreateTable(cap, table string, columns []ColumnDef) error` — erstellt `cap_{name}__{table}`. |
| `CapsUpsert` | `func (s *Store) CapsUpsert(cap, table string, data map[string]any) error` |
| `CapsQuery` | `func (s *Store) CapsQuery(cap, table, where string, args []any, limit int) ([]map[string]any, error)` |
| `CapsQueryPaged` | `func (s *Store) CapsQueryPaged(cap, table, where string, args []any, limit, offset int) (QueryResult, error)` — mit Pagination-Metadata. |
| `CapsDelete` | `func (s *Store) CapsDelete(cap, table, where string, args []any) (int64, error)` — gibt affected rows zurück. |
| `CapsListTables` | `func (s *Store) CapsListTables(cap string) ([]TableInfo, error)` |

Hilfstypen: `ColumnDef`, `TableInfo`, `QueryResult`.
Interne Hilfsfunktionen: `resolveTableName`, `sanitizeWhere`, `createCapStoreSchema`.

**Datei: `internal/storage/schema.go`**

Schema-Migration für `session_active_caps` und `cap_store_meta`. v0.55-Migration benannte `session_active_capabilities` → `session_active_caps` und `capability_name` → `cap_name` um.

### MCP (Tool-Exposition)

**Datei: `internal/mcp/server.go`**

Registriert MCP-Tools (Claude Code sieht sie als `mcp__yesmem__<name>`):

| Tool-Name | Beschreibung |
|-----------|-------------|
| `activate_cap` | Aktiviert Cap für Thread. Gibt registerTool()-JS zurück. |
| `deactivate_cap` | Entfernt Cap aus Thread-State. |
| `get_caps` | Listet alle Capabilities (Filter: name, tag, project). |
| `save_cap` | Speichert/updated Capability. Auto-Supersede bei gleichem Namen. |
| `register_caps` | Batch-Hydration: generiert registerTool()-JS für mehrere Caps. |
| `cap_store` | Generischer KV-Store. Actions: create_table, upsert, query, delete, list_tables, table_exists. |
| `get_active_caps` | Interne Abfrage: aktive Caps eines Threads (für Proxy-Query, nicht User-facing). |

**Datei: `internal/mcp/proxy.go`**

Weiterleitung an Daemon via Unix Socket. Kein Capability-spezifischer Code — generisches RPC-Relay.

### Briefing

**Datei: `internal/briefing/briefing.go`**

| Funktion | Signatur |
|----------|----------|
| `renderCaps` | `func renderCaps(caps []CapEntry) string` — rendert Caps im Briefing-Text (älterer Pfad via Learnings). |

---

## 5. MCP-Tools (Parameter-Details)

| Tool | Parameter | Beschreibung |
|------|-----------|--------------|
| `activate_cap` | `name` (required), `project?` | Aktiviert Cap für Session. Gibt `registerTool()`-JS zurück. `thread_id` wird automatisch injiziert. |
| `deactivate_cap` | `name` (required) | Entfernt Cap aus Session-State. Thread-ID auto-injiziert. |
| `get_caps` | `project?`, `name?`, `tag?`, `limit?` | Listet alle Capabilities (aus Learnings mit category=cap). |
| `save_cap` | `name`, `description`, `handler_repl?`, `handler_bash?`, `schema?`, `tags?`, `tested?`, `auto_active?` (default: **true**), `project?` | Speichert/updated Capability. Auto-Supersede bei gleichem Namen. `auto_active` wird in CapMeta-JSON gespeichert. Default true — Caps sind standardmässig sofort für alle Sessions verfügbar. Explizit `false` setzen um eine Cap nur bei manueller Aktivierung verfügbar zu machen. |
| `register_caps` | `names?`, `project?` | Batch-Hydration: generiert `registerTool()`-JS für mehrere Caps gleichzeitig. |
| `cap_store` | `capability`, `action`, `table?`, `columns?`, `data?`, `where?`, `args?`, `limit?`, `offset?` | Generischer KV-Store. Actions: `create_table`, `upsert`, `query`, `delete`, `list_tables`, `table_exists`. |
| `get_active_caps` | `thread_id` (auto-injected) | Interne Proxy-Query. Gibt aktive Caps eines Threads zurück. |

---

## 6. Proxy-Pipeline-Integration

Die Capability-Injection fügt sich in die bestehende Proxy-Pipeline ein:

```
Request eingehend
  → StripReminders
  → CompressContext
  → CalcCollapseCutoff
  → CollapseOldMessages / Stubify
  → ReplaceSystemBlock
     └─ renderCapabilitiesCatalog()    ← Katalog im system-reminder (caps_inject.go)
  → StripOldNarratives
  → reexpandStubsFor
  → injectCapabilitiesTurn()           ← Aktive Caps an req["tools"] (caps_inject.go)
  → UpgradeCacheTTL / EnforceCacheBreakpointLimit
Request an Anthropic API
```

**Katalog-Injection** (`renderCapabilitiesCatalog` in `caps_inject.go`):
- Erzeugt `<capabilities-available>` Block als system-reminder
- Enthält Bootstrapper: `registerTool("activate_cap", ...)` als REPL-Tool
- Bootstrapper ruft intern `mcp__yesmem__activate_cap` (⚠ siehe Naming-Diskrepanz)
- Listet alle verfügbaren Caps mit Name + Beschreibung als Tabelle
- Wird einmal pro Session gerendert

**Schema-Injection** (`injectCapabilitiesTurn` in `caps_inject.go`):
- Liest aktive Caps via `cachedQueryFn` (Cache + Daemon-Fallback)
- Hängt JSON-Schemata an `req["tools"]` im API-Request an
- Native Tools haben Vorrang bei Name-Kollision (Skip mit Warn-Log)

**Cache** (`CapsCache` in `caps_cache.go`):
- In-Memory-Cache pro Thread-ID
- Wird invalidiert via `invalidateThreadCaches()` (invalidiert auch frozenStubs + briefingCache)
- `cachedQueryFn()` wraps Daemon-Query: cached nur `get_active_caps` Responses

---

## 7. Blob-Pipe (>30KB Payloads)

Für Capabilities die HTTP-Fetches > 30KB brauchen (z.B. Reddit-Posts mit vielen Kommentaren).

**Problem:** REPL-Output wird bei ~30KB abgeschnitten. Tempfiles brauchen Read-Tool-Permission.

**Lösung:** CLI-Subcommands `cap-blob-put` / `cap-blob-get`:

```
Producer → curl | yesmem cap-blob-put --cap NAME --key KEY
                    ↓
              cap_{NAME}__blobs (60KB chunks, auto-created)
                    ↓
Consumer → cap_store({action:"query", table:"blobs", ...})
                    ↓
              rows.map(r => r.data).join('')  → vollständiger Payload
```

**Package:** `internal/capblob/` (blob.go, blob_test.go). CLI: `yesmem cap-blob-put --cap NAME --key KEY`. Verwendet von `reddit_fetch`.

---

## 8. REPL Pattern Detection (Wiederholungsmuster → Cap-Vorschlag)

Erkennt wiederholt ausgeführte Shell-Commands und schlägt vor, daraus Capabilities zu bauen. Das Feature bildet die Brücke zwischen ad-hoc REPL-Nutzung und persistentem Werkzeugwissen.

### Konzept

```
Session N: sh('curl ... | jq ...')
Session N+1: sh('curl ... | jq ...')    ← selbes Muster
Session N+2: sh('curl ... | jq ...')    ← Pattern count = 3
...
Session N+4: sh('curl ... | jq ...')    ← count ≥ 5 → Suggestion!

Proxy injiziert Hint: "Du hast dieses Muster 5× verwendet.
  Erwäge eine Capability daraus zu bauen mit save_cap(...)."
```

### Wie es funktioniert

1. **Normalisierung** (`repl_pattern.go`): Commands werden zu "Shape Hashes" normalisiert — variable Teile (URLs, IDs, Timestamps) werden durch Platzhalter ersetzt, sodass `curl https://api.example.com/v1/users/123` und `curl https://api.example.com/v1/users/456` denselben Hash ergeben.

2. **Recording** (Proxy → Daemon): Jeder REPL/Bash-Tool-Call wird vom Proxy an den Daemon gemeldet (`record_repl_pattern`). Der Daemon speichert Shape-Hash + Rohbefehl + Projekt + Timestamp in `repl_patterns` Tabelle.

3. **Detection** (`repl_pattern_detect.go`): Proxy prüft pro Request ob ein Pattern-Vorschlag fällig ist. Triggert wenn:
   - Shape-Hash ≥ `minCount` (Default: 5) mal für das Projekt gesehen
   - Pattern noch nicht als Cap gespeichert
   - Pattern nicht dismissed
   - Pattern nicht "trivial" (Filter via `isTrivialShape`)

4. **Suggestion**: Proxy injiziert einen Hint-Text in die nächste Antwort, der vorschlägt das Pattern als Capability zu speichern.

5. **Dismiss**: User kann Vorschlag ablehnen. Nach 3 Dismissals wird das Pattern permanent ignoriert.

### Trivial-Shape-Filter

Filtert Commands die zu generisch sind um als Capability sinnvoll zu sein:

- Reine `cd`, `ls`, `cat`, `echo` Commands
- Einfache `git status`, `git log`, `git diff` ohne komplexe Flags
- `grep` ohne Pipeline

### Komponenten

**Datei: `internal/proxy/repl_pattern.go`**

| Symbol | Art | Beschreibung |
|--------|-----|-------------|
| `PatternShape` | struct | Felder: `Hash`, `Normalized`, `Raw`, `Tokens` |
| `NormalizeCommand` | func | `func NormalizeCommand(cmd string) PatternShape` — normalisiert Command zu Shape-Hash. |
| `isTrivialShape` | func | `func isTrivialShape(shape PatternShape) bool` — filtert triviale Commands. |

**Datei: `internal/proxy/repl_pattern_detect.go`**

| Symbol | Art | Beschreibung |
|--------|-----|-------------|
| `PatternSuggestion` | struct | Felder: `Hash`, `Raw`, `Count`, `Project` |
| `detectReplPattern` | func/method | Prüft ob ein Pattern-Vorschlag injiziert werden soll. Abfrage an Daemon `get_repl_patterns`. |
| `formatPatternSuggestion` | func | Erzeugt den Hint-Text für den Vorschlag. |

**Datei: `internal/daemon/handler_repl_patterns.go`**

| Funktion | Beschreibung |
|----------|-------------|
| `handleRecordReplPattern` | Speichert Shape-Hash + Raw-Command + Projekt in DB. |
| `handleGetReplPatterns` | Gibt Patterns mit count ≥ threshold für ein Projekt zurück. |
| `handleDismissReplPattern` | Markiert Pattern als dismissed. Permanent nach 3×. |

**Datei: `internal/storage/repl_patterns.go`**

| Funktion | Beschreibung |
|----------|-------------|
| `RecordReplPattern` | `func (s *Store) RecordReplPattern(hash, normalized, raw, project string) error` |
| `GetReplPatterns` | `func (s *Store) GetReplPatterns(project string, minCount int) ([]ReplPattern, error)` |
| `DismissReplPattern` | `func (s *Store) DismissReplPattern(hash, project string) error` |
| `IsPatternDismissed` | `func (s *Store) IsPatternDismissed(hash, project string) (bool, error)` |

Schema: `repl_patterns` Tabelle (hash, normalized, raw, project, count, first_seen, last_seen, dismissed_count).

### MCP-Tools

| Tool | Beschreibung |
|------|-------------|
| `record_repl_pattern` | Intern, vom Proxy aufgerufen. Nicht User-facing. |
| `get_repl_patterns` | Gibt frequentierte Patterns zurück. |
| `dismiss_repl_pattern` | User lehnt Vorschlag ab. |

### Noise Reduction (live seit 2026-04-20)

| Maßnahme | Datei | Effekt |
|----------|-------|--------|
| **Deny-List erweitert** (+15: git, mkdir, rm, cp, mv, touch, chmod, chown, ln, export, source, exit, clear, history, wc) | `repl_pattern.go` | Triviale Shell-Commands werden nicht gezählt |
| **Session-Budget max 3** — `patternBudget map[string]int` auf Server struct | `proxy.go` | Nach 3 Suggestions pro Thread ist Schluss |
| **Threshold 5→8** — Pattern muss sich 8× wiederholen | `handler_repl_patterns.go` | Filtert kurzfristige Wiederholungen |

Verifiziert: 3 Suggestions statt 40+ pro REPL-intensiver Session.

### Response-Format (geändert 2026-04-22)

`get_repl_pattern_suggestion` gibt ein Envelope-Format zurück:
```json
{"pattern": {"shape_hash": "...", "count": 8, ...}, "workflow": {"sequence_hash": "...", "count": 3, ...}}
```
Proxy parst via Envelope-Struct mit Guard gegen leere `shape_hash`.

---

## 8b. Multi-Turn Workflow Sequence Detection (2026-04-22)

Erkennt wiederkehrende Tool-Call-Sequenzen über Turns hinweg und schlägt Cap-Bundling vor.

### Architektur

- **Eine Tabelle** `thread_sequences` (thread_id PK, project, turn_hashes JSON max 20 FIFO, updated_at)
- **Turn-Hash** = Tool-Typ-Namen aus Assistant-Turn → consecutive Duplikate entfernen → join mit `→` → SHA256[:16]
- **Workflow-Matching** = on-demand, kein Ticker. 3er-Subsequenzen in-memory aus allen Thread-Sequences desselben Projekts extrahieren, zählen, bei count ≥ 3 über verschiedene Threads → Suggestion
- **Budget** = teilt `patternBudget` (max 3/Thread) mit Single-Command-Pattern-Suggestions

### Dateien

| Datei | Inhalt |
|-------|--------|
| `internal/storage/turn_sequences.go` | Schema, RecordTurnHash (FIFO upsert), GetWorkflowSuggestions |
| `internal/storage/turn_sequences_test.go` | 7 Tests (FIFO, Append, Scope, Subsequenz, False-Positive) |
| `internal/proxy/turn_sequence.go` | ExtractToolTypes, ComputeTurnHash, computeTurnHashFromMessages |
| `internal/proxy/turn_sequence_test.go` | 7 Tests (Dedup, Empty, Length, Extraction) |

### Datenfluss

1. Proxy empfängt Request mit User-Message
2. Extrahiert Tool-Typen aus dem letzten Assistant-Turn vor der User-Message
3. Berechnet Turn-Hash (dedupliziert, 16-char)
4. Sendet async per RPC `record_turn_sequence` an Daemon
5. Daemon upsert in `thread_sequences` (FIFO Ring-Buffer, max 20)
6. Bei `get_repl_pattern_suggestion` lädt Daemon alle Sequences des Projekts, extrahiert 3er-Subsequenzen, gibt count ≥ 3 als `workflow` im Envelope zurück

---

## 9. Fixation Detector (Endlos-Schleifen-Erkennung)

Erkennt wenn Claude in einer unproduktiven Schleife feststeckt — z.B. denselben fehlschlagenden Build wiederholt, oder dieselbe Datei immer wieder editiert.

### Drei Fixation-Signale

| Signal | Schwellwert | Beschreibung |
|--------|-------------|-------------|
| Consecutive Error Runs | ≥ 8 | Aufeinanderfolgende Tool-Calls die mit Fehler enden. |
| Edit-Build-Error Cycles | ≥ 6 | Wiederholtes Muster: Datei editieren → Build/Test → Fehler → selbe Datei editieren. |
| Excessive File Retries | ≥ 10 | Dieselbe Datei wird ≥10× editiert innerhalb einer Sequenz. |

### Komponenten

**Datei: `internal/proxy/fixation_detector.go`**

| Symbol | Art | Beschreibung |
|--------|-----|-------------|
| `FixationResult` | struct | Felder: `IsFixated bool`, `Ratio float64`, `Signal string`, `Details string` |
| `DetectFixation` | func | `func DetectFixation(messages []Message) FixationResult` — analysiert Message-History auf Fixation-Signale. |
| `countConsecutiveErrors` | func | Zählt aufeinanderfolgende Fehler-Tool-Calls. |
| `detectEditBuildCycles` | func | Erkennt Edit → Build → Error Zyklen. |
| `countFileRetries` | func | Zählt Edits pro Datei. |

### Integration in Proxy

Der Fixation Detector wird in der Proxy-Pipeline aufgerufen. Bei erkannter Fixation wird ein Hint injiziert der Claude auffordert, die Strategie zu wechseln.

---

## 10. Existierende Capabilities (Beispiele)

Im System registrierte Capabilities (Stand 2026-04-21):

| Capability | Beschreibung | Typ |
|------------|-------------|-----|
| `reddit_fetch` | Reddit-Post + Kommentare + Links abrufen | handler_repl |
| `reddit_search` | Reddit durchsuchen, Ergebnisse klassifizieren + persistieren | handler_repl |
| `cap_search` | Generische Suche über store()-Primitive Tabellen | handler_repl |
| `cap_collect` | Collect-and-prep über store()-Primitive für Analyse | handler_repl |
| `cap_save_analysis` | Analyse-Ergebnisse append-only persistieren | handler_repl |
| `reddit_research` | Topic-Research: parallel search, fetch top posts, haiku()-Klassifizierung + Synthese | handler_repl (composite) |
| `cap_delete` | Capability komplett entfernen (learnings DB + cap_store tables) | handler_repl |
| `proxy_health` | Proxy/Daemon Health aus journalctl, Errors zählen, in cap_store speichern | handler_repl |

---

## 11. Commit-History (chronologisch)

```
2026-04-15  feat(models): add 'capability' category (later migrated to 'cap')
2026-04-15  feat(daemon): CapMeta type (cap_meta.go)
2026-04-15  feat(daemon): handleGetCaps / handleSaveCap
2026-04-15  feat(daemon): handleRegisterCaps (Batch-Hydration)
2026-04-16  feat(briefing): renderCaps() + Tests
2026-04-16  feat(mcp): register get_caps/save_cap/register_caps tools
2026-04-17  feat(storage): session_active_caps table + methods
2026-04-17  feat(daemon): handleActivateCap/handleDeactivateCap handlers
2026-04-17  feat(mcp): activate_cap/deactivate_cap tools
2026-04-18  feat(storage): cap_store — separate capabilities.db + CRUD
2026-04-18  feat(daemon): handleCapStore handler + sandboxing + quotas
2026-04-18  feat(mcp): cap_store MCP tool
2026-04-18  feat(proxy): injectCapabilitiesTurn + CapsCache
2026-04-20  feat(proxy): renderCapabilitiesCatalog + Bootstrapper
2026-04-20  feat(proxy): capabilities lazy-activation catalog + API-actual threshold
2026-04-21  fix(daemon): auto_active default true for save_cap
2026-04-22  feat(capfile): remove Notes from struct/parser/writer
2026-04-22  feat(capfile): DetectRequires scans script for cap_store/blob_put/blob_get
2026-04-22  feat(capfile): adapter registry with bidirectional name mapping
2026-04-22  feat(capfile): writer converts provider-specific to generic names
2026-04-22  feat(daemon): adapter mapping in activate_cap and save_cap handlers
2026-04-22  fix(caps): use already-constructed meta for WriteCapToDisk (6ed3fe5)
2026-04-22  feat(proxy): multi-turn workflow sequence detection (81eb6b6)
2026-04-22  fix(proxy): parse nested pattern envelope in suggestion response (6f7b9da)
```

---

## 12. Implementierungsstand

### Erledigt

- [x] `cap` als gültige Learning-Kategorie (v0.55 Migration von `capability`)
- [x] `CapMeta` Struct mit `ParseCapMeta`/`ToJSON`/`HasTag` (`cap_meta.go`)
- [x] Daemon-Handler: `handleGetCaps`, `handleSaveCap`, `handleRegisterCaps`, `handleActivateCap`, `handleDeactivateCap`, `handleGetActiveCaps`
- [x] Cap Store: separate `capabilities.db`, CRUD, Sandboxing, Quotas, Paging
- [x] MCP-Tool-Registrierung: `get_caps`, `save_cap`, `register_caps`, `activate_cap`, `deactivate_cap`, `cap_store`, `get_active_caps`
- [x] `session_active_caps` Tabelle + Storage-Methods (`ActivateCap`, `DeactivateCap`, `GetSessionCaps`, `TouchCap`)
- [x] Cap Store Storage: `CapsCreateTable`, `CapsUpsert`, `CapsQuery`, `CapsQueryPaged`, `CapsDelete`, `CapsListTables`, `OpenCapsDB`, `CloseCapsDB`, `CapsReady`, `ValidateCapName`
- [x] Proxy: `injectCapabilitiesTurn()` + `injectCapabilitiesTurnImpl()` — Schema-Injection für aktive Caps
- [x] Proxy: `CapsCache` — In-Memory-Cache mit `Get`/`Set`/`Invalidate` + `cachedQueryFn`
- [x] Proxy: `renderCapabilitiesCatalog()` — Katalog im system-reminder mit Bootstrapper
- [x] Proxy: `invalidateThreadCaches()` — koordinierte Cache-Invalidierung
- [x] Briefing: `renderCaps()` — Caps im Session-Briefing
- [x] Bestehende Capabilities: reddit_fetch, reddit_search, cap_search, cap_collect, cap_save_analysis, reddit_research, cap_delete, proxy_health
- [x] REPL Pattern Detection: `NormalizeCommand`, `isTrivialShape`, `detectReplPattern`, `formatPatternSuggestion`
- [x] Storage: `RecordReplPattern`, `GetReplPatterns`, `DismissReplPattern`, `IsPatternDismissed`
- [x] Daemon-Handler: `handleRecordReplPattern`, `handleGetReplPatterns`, `handleDismissReplPattern`
- [x] MCP-Tools: `record_repl_pattern`, `get_repl_patterns`, `dismiss_repl_pattern`
- [x] Fixation Detector: `DetectFixation` mit 3 Signalen (consecutive errors, edit-build cycles, file retries)
- [x] Trivial-Shape-Filter für REPL Pattern Detection (letzter Commit `4708c8d`)
- [x] REPL Pattern Noise Reduction: Deny-List +15, Budget max 3/Thread, Threshold 5→8
- [x] REPL Pattern Suggestion Envelope-Format: `{"pattern": {...}, "workflow": {...}}`
- [x] Multi-Turn Workflow Sequence Detection: `thread_sequences` Tabelle, Turn-Hash-Berechnung, Subsequenz-Matching (Commit `81eb6b6`)
- [x] `auto_active` Default auf `true` geändert (`handler_caps.go`)
- [x] Blob-Pipe (`internal/capblob/`): CLI `cap-blob-put`, cap_store-basierter Chunk-Store, verwendet von `reddit_fetch`
- [x] CAP.md: Notes-Section entfernt aus Parser, Writer, Struct
- [x] CAP.md: `DetectRequires()` — scannt Script nach `store(`/`web(`/`file(` und populiert `Requires []string`
- [x] Adapter-Registry: `DefaultAdapters()`, `ProviderToGeneric()`, `GenericToProvider()`, `GenerateAdapterJS()` (`adapter.go`)
- [x] Writer wendet `ProviderToGeneric` vor Render an — CAP.md-Dateien speichern generische Namen
- [x] Daemon: `save_cap` normalisiert handler_repl via `ProviderToGeneric`, `activate_cap`/`register_caps` expandieren via `GenericToProvider`
- [x] Existierende CAP.md-Dateien auf generische Namen migriert
- [x] WriteCapToDisk Bug gefixt: meta-Objekt direkt übergeben statt Content-String re-parsen (Commit `6ed3fe5`)
- [x] ExportAllCaps + SyncCapsFromDisk bei Daemon-Start (`daemon.go:193-194`)
- [x] yesmem-build-tool Skill: Dependency-Caps-Doku, haiku()-Note, Composite-Example, API-Rename, Bundled-Template synced

### Offen

- [ ] **Split-Brain Session-ID** (#53443) — Proxy thread_id vs. Daemon session_id divergieren über pidMap. Blockt korrekten End-to-End Flow für Subagents. **Kritischster offener Punkt.**
- [ ] **Subagent-Injection** (#53524) — Subagents empfangen keine `<capabilities-active>` Blöcke trotz `auto_active=true` in CapMeta. Hängt vermutlich am Session-ID-Mapping.
- [ ] **Phase 2 Idempotency** — activate_cap Over-Matching: Erkennung "bereits injiziert" ist zu breit.
- [ ] **Eviction/TTL** — Caps bleiben bis Session-Ende aktiv. V2 nachrüsten falls Friction sichtbar.
- [ ] **SSE-Weights LFS** (#53273) — Embedding-Tests rot wegen ASCII statt Binary (pre-existing, nicht capabilities-spezifisch).
- [x] **cap_store→store Rename Migration** — Alle Caps verwenden bereits generisches `store()`. `GenericToProvider` konvertiert bei Aktivierung korrekt zu `mcp__yesmem__cap_store()`. Verifiziert 2026-04-22.
- [x] **Self-Improving Cap Cycle** — Via Proxy-Instruktion in `caps_inject.go:71`: "When a capability handler errors: diagnose the root cause, fix the handler, save the corrected version via save_cap (auto-supersedes), then retry." Kein Daemon-Prozess nötig.
- [x] **cap export/import CLI** — Export läuft bei Daemon-Start via `ExportAllCaps`. Import implizit: Caps im `~/.claude/caps/` Verzeichnis werden bei Start via `SyncCapsFromDisk` eingelesen. Kein CLI-Subcommand nötig.
- [ ] **Workflow Suggestion Injection** — Workflow-Suggestions werden im Envelope zurückgegeben, aber der Proxy formatiert und injiziert sie noch nicht als system-reminder. Aktuell nur Daten-Sammlung aktiv.
- [ ] **Worktree → main Merge** — Der gesamte feat+capability-memory Branch ist nicht auf main gemergt. Alle Features laufen nur über das deployed Binary aus dem Worktree.

---

## 13. Design-Entscheidungen

| Entscheidung | Begründung |
|-------------|-----------|
| Capabilities als Learnings (category="cap"), keine eigene Tabelle | Nutzt bestehende Supersede/Search/Embed-Infrastruktur. Kein Schema-Migration-Aufwand. |
| Deliberative Aktivierung statt Auto-Injection | Modell wählt zuverlässiger als Embedding-Ranker. Keine False Positives. Weniger Token-Kosten. |
| Native Tools gewinnen bei Name-Kollision | Sicherheit: native MCP-Tools dürfen nie von Capabilities verdeckt werden. |
| Separate capabilities.db für Cap Store | Isolation von yesmem.db. Keine Schema-Pollution. Unabhängiges Locking. |
| Blob-Pipe statt Tempfiles | Kein Read-Tool-Permission-Prompt. Kein /tmp-Cleanup. Persistenter als Tempfiles. |
| Cap Store Sandboxing (kein DROP/ALTER) | Capabilities sollen Daten schreiben, nicht Struktur zerstören. Defensive Architektur. |
| Bootstrapper als registerTool() im Katalog | Ermöglicht `activate_cap` als REPL-Tool ohne vorherige MCP-Round-Trip. Selbst-bootstrappend. |
| v0.55 Rename capability→cap | Konsistenz: kürzere, einheitliche Namenspräfixe. Alle APIs nutzen `cap_*`. |
| auto_active Default true | Neue Caps sollen standardmässig sofort verfügbar sein. Opt-out statt Opt-in — reduziert Friction beim Cap-Building-Flow. |

---

## 14. CAP.md — File-basierte Cap-Definitionen

### Konzept

Jede Capability hat eine `CAP.md`-Datei als menschenlesbares, editierbares Source-of-Truth. Die Datei beschreibt was der Cap tut, wie er es tut (Script), und welche Daten er speichert (Database).

SQLite bleibt Runtime-Store — Files werden bei Daemon-Start in die DB gesynct.

### Dateiformat

```markdown
---
name: reddit_search
description: "Search Reddit by topic"
version: 2
tags: [web, reddit]
runtime: repl
scope: user
tested: true
auto_active: true
---

## Purpose
Prose: was der Cap tut.

## Script
```javascript
async function handler({ query, limit = 10 }) { ... }
```

## Database
```sql
CREATE TABLE IF NOT EXISTS listings (
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL
);
```

**Frontmatter:** `name` und `description` sind Pflichtfelder. `runtime` wird aus der Script-Sprache abgeleitet (`javascript` → repl, `bash` → bash). `schema` wird aus der Funktionssignatur abgeleitet und nicht im Frontmatter gespeichert.

**Sections:** Reihenfolge ist fix: Purpose → Script → Database. Database ist optional (Section darf leer sein).

**SQL-Validation:** Nur `CREATE TABLE/INDEX/VIEW/TRIGGER IF NOT EXISTS` erlaubt. `DROP`, `ALTER`, `INSERT`, `UPDATE`, `DELETE` werden rejected.

### Verzeichnisstruktur

```
~/.claude/caps/
├── deploy/
│   └── CAP.md
├── reddit_search/
│   └── CAP.md
├── reddit_fetch/
│   └── CAP.md
└── cap_collect/
    └── CAP.md
```

User-Scope: `~/.claude/caps/<name>/CAP.md`
Project-Scope: `<project>/.claude/caps/<name>/CAP.md` (geplant)

### Daemon-Integration

Bei jedem Daemon-Start laufen zwei Phasen:

1. **File → DB** (`SyncCapsFromDisk`): Alle CAP.md Files werden gelesen, geparst, und via `save_cap` in die DB upserted.
2. **DB → File** (`ExportAllCaps`): Alle DB-Caps ohne CAP.md File werden als Files exportiert. DDL wird aus `caps.db` via `sqlite_master` gelesen.

Bei jedem `save_cap` MCP-Call: nach DB-Upsert wird die CAP.md automatisch geschrieben/aktualisiert.

### Dev-Workflow

1. `~/.claude/caps/my_cap/CAP.md` anlegen oder editieren
2. Daemon neu starten (`make deploy` oder `systemctl restart yesmem-daemon`)
3. Änderung ist sofort in der DB und über MCP-Tools verfügbar

### Package-Struktur

| File | Funktion |
|------|----------|
| `internal/capfile/parse.go` | Parser: YAML-Frontmatter + 3 Sections (Purpose, Script, Database), Schema-Derivation aus JS-Signatur |
| `internal/capfile/write.go` | Writer: Canonical CAP.md, SQL-Formatierung, JS-Formatter, Atomic Write. Wendet `ProviderToGeneric` auf Script an |
| `internal/capfile/scanner.go` | Scanner: Directory-Discovery, `ScanAll()` über User + Project Dirs |
| `internal/capfile/adapter.go` | Adapter-Registry: `DefaultAdapters()`, `ProviderToGeneric()`, `GenericToProvider()`, `GenerateAdapterJS()` |
| `internal/daemon/cap_sync.go` | Integration: `CapFileToParams()`, `CapMetaToCapFile()`, `WriteCapToDisk()`, `SyncCapsFromDisk()`, `ExportAllCaps()` |
| `internal/storage/cap_store.go` | `GetCapTableDDL()`: DDL aus `sqlite_master` für Database-Section |
| `docs/CAPS-md-spec.md` | Format-Spezifikation |

### Adapter-Layer (Provider-Abstraktion)

CAP.md-Dateien und die DB speichern **generische** Funktionsnamen für Portabilität. Bei der Aktivierung werden diese in **provider-spezifische** Implementierungen übersetzt.

Es gibt **3 Adapter-Primitives**, jeweils action-basiert:

**Direct Mapping** (`AdapterConfig.Direct`):

| Generisch | Provider (YesMem MCP) | Actions |
|-----------|----------------------|---------|
| `store()` | `mcp__yesmem__cap_store()` | `create_table`, `upsert`, `query`, `delete`, `list_tables`, `blob_put`, `blob_get` |

**Dispatcher Mapping** (`AdapterConfig.Dispatchers`):

| Generisch | Action | Provider-Implementierung |
|-----------|--------|------------------------|
| `web()` | `fetch` | `sh('curl ...')` |
| `web()` | `search` | `WebSearch()` |
| `file()` | `read` | `cat()` |
| `file()` | `write` | `put()` |
| `file()` | `glob` | `gl()` |

**Roundtrip:**

1. **save_cap** (User/Daemon → DB): `ProviderToGeneric()` normalisiert handler_repl (nur Direct-Mappings: `mcp__yesmem__cap_store(` → `store(`)
2. **Writer** (DB → CAP.md): `ProviderToGeneric()` normalisiert Script vor Render
3. **activate_cap / register_caps** (DB → Claude): `GenericToProvider()` expandiert Direct-Mappings zurück
4. **GenerateAdapterJS()**: Erzeugt Direct-Shims (`globalThis.store = async(a) => mcp__yesmem__cap_store(a)`) + Dispatcher-Shims (`globalThis.web = async({action,...p}) => { const d = {fetch: ..., search: ...}; return d[action](p); }`)

**Design-Prinzipien:**
- `store()` ist 1:1 (String-Replace, wie bisher)
- `web()` und `file()` sind Dispatcher — das Cap schreibt `web({action:'fetch', url:'...'})`, der JS-Shim dispatcht zur Laufzeit
- Runtime-Builtins (`sh()`, `haiku()`, `log`, `JSON`) sind KEINE Adapter — die sind immer verfügbar
- Wer CC-spezifische Tools direkt will (`WebFetch`, `Read`), schreibt die hin — nur nicht portabel

**Warum:** Ein Cap, der `store()` nutzt, funktioniert unverändert wenn der MCP-Server umbenannt wird. Nur `DefaultAdapters()` muss angepasst werden. `web()`/`file()`-Dispatcher können auf andere Backends zeigen (z.B. `playwright` statt `curl`) ohne das Cap-Script zu ändern.

### Gotchas

- **YAML-Description quoten**: Descriptions mit `:`, Backticks, oder Sonderzeichen müssen in `%q`-Quotes stehen, sonst YAML-Parse-Error.
- **Schema nicht im Frontmatter**: Wird aus der JS-Funktionssignatur abgeleitet. Explizites Schema nur wenn Signatur-Derivation nicht reicht.
- **SQL-Validation eigene Allowlist**: `storage.blockedSQLPattern` blockt ALLES inkl. CREATE — die Database-Section hat eine eigene Validation (`dangerousSQLPattern` + `safeSQLPattern` in `capfile/parse.go`).
- **formatJS nur für Einzeiler**: Mehrzeilige Scripts werden nicht reformatiert — der naive Formatter zerstört Destructuring-Parameter.
- **Reihenfolge beim Start**: Erst `SyncCapsFromDisk` (File→DB), DANN `ExportAllCaps` (DB→File). Umgekehrt würden hand-editierte Files überschrieben.

---

## 15. Quelldokumente

| Dokument | Pfad |
|----------|------|
| Original-Spec | `docs/superpowers/specs/2026-04-15-capability-memory-design.md` |
| Phase 1+2 Plan | `yesdocs/superpowers/plans/2026-04-15-capability-memory-phase-2.md` |
| Lazy-Activation Plan | `yesdocs/plans/2026-04-17-capability-lazy-activation.md` |
| Blob-Pipe Plan | `yesdocs/plans/2026-04-18-cap-blob-pipe.md` |
| Phase 3 Cap Store Plan | `.claude/plans/phase3-cap-store.md` |
| Cap Store Analysis | `docs/cap-store-analysis.md` |
| Cap Store Examples | `docs/cap-store-analysis-examples.md` |

---

## 16. Daemon Scheduler

Cron-based task scheduler built into the daemon. Defines recurring or one-shot jobs that automatically spawn agents.

### Two Execution Modes

| Mode | Mechanism | Visible? | Overhead | Use case |
|------|-----------|----------|----------|----------|
| `agent` | PTY bridge + tmux window | Yes | Full briefing + agent lifecycle | Complex tasks, debugging, coding plans |
| `headless` | `claude -p` as subprocess | No | Minimal — no lifecycle management | Routine automation, cron jobs, data collection |

### MCP Interface

Single `schedule` tool with four actions:

| Action | Parameters | Description |
|--------|-----------|-------------|
| `create` | name, cron, prompt, mode, enabled, recurring | Create job |
| `list` | — | List all jobs |
| `delete` | id | Delete job |
| `run` | id, mode, prompt | Manual trigger |

### Task Delivery

The scheduler writes the task prompt to a job-specific scratchpad section **before** spawning the agent. The agent reads its task from the briefing — no relay timing issues.

```
Section: sched-<job-name>
Content: ## SCHEDULED TASK [<job-name>]
         Job-ID: <id>
         <prompt with focus instructions>
```

### Agent Lifecycle (mode `agent`)

- **Pre-spawn cleanup** — stops existing agent on the same section
- **Idle timeout** — 10 minutes, unified across all agent states (running, frozen, idle)
- **Watchdog goroutine** — polls agent status every 30 seconds

### Headless Mode

Uses `claude -p` (Claude Code non-interactive mode) as a daemon subprocess:
- Full MCP tool access (caps, cap_store, haiku, scratchpad)
- Runs through the proxy (subscription-based, no API key needed)
- Output captured and written to scratchpad
- No tmux window, no PTY bridge, no watchdog needed
- ~2x faster than agent mode with comparable results

### Caps as Automation Primitives

Caps are ideal for scheduled tasks because they are predictable: defined schema, known handler, deterministic behavior. The agent activates the cap and executes it — no improvisation needed.

### Comparison with Anthropic Scheduled Tasks

| | Anthropic Cloud Routines | Desktop Scheduled Tasks | YesMem Scheduler |
|---|---|---|---|
| Runs on | Anthropic cloud | Local (app open) | Local (daemon) |
| Memory | None (fresh each run) | None | Full persistent memory |
| Local files | No (fresh clone) | Yes | Yes |
| MCP servers | Connectors only | Local | Full local MCP |
| Caps/Tools | N/A | N/A | Reusable caps + cap_store |
| Cost | API tokens + $0.08/h | Subscription | Subscription |
| Limits | Pro: 5/day, Max: 15/day | Desktop-bound | Unlimited (self-hosted) |

### Components

| File | Symbols |
|------|---------|
| `internal/daemon/scheduler.go` | `ScheduledJob`, `Scheduler`, `JobExecutor`, `AddJob`, `Tick` |
| `internal/daemon/handler_scheduler.go` | `handleSchedule`, `scheduleCreate/List/Delete/Run`, `executeScheduledPrompt`, `executeAgent`, `executeHeadless`, `watchScheduledAgent` |
| `internal/storage/scheduler.go` | `ScheduledJobRow`, `SaveScheduledJob`, `ListScheduledJobs`, `DeleteScheduledJob`, `UpdateJobLastRun` |
