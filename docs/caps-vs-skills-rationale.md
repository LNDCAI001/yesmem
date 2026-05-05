# Caps vs. Skills — Warum ein eigenes Format

## Zusammenfassung

Capabilities (Caps) nutzen `CAP.md` in `~/.claude/caps/` statt `SKILL.md` in `~/.claude/skills/`. Dieses Dokument begründet die Entscheidung.

## Was sind Caps, was sind Skills?

| | Skills | Caps |
|---|---|---|
| **Zweck** | Anleitungen — wie man etwas tut | Werkzeuge — tun etwas |
| **Mechanismus** | Markdown-Instruktionen, von Claude gelesen | `registerTool()` Code, im REPL ausgeführt |
| **Beispiel** | "Wie schreibt man gute Commits" | `reddit_fetch({url})` → strukturierte Daten |
| **Lebenszyklus** | Geladen bei Trigger, dann verworfen | Registriert, bleibt als Tool verfügbar |
| **Persistenz** | Keine (Datei wird jedes Mal gelesen) | DB-gestützt (überlebt Sessions) |

## Experiment: Cap als SKILL.md

Ein Cap (reddit_fetch) wurde als `SKILL.md` in `~/.claude/skills/` abgelegt. Ergebnis:

**Was funktioniert:**
- Claude Code entdeckt die Datei automatisch
- Description erscheint in der Skill-Liste
- Invocation via `/reddit_fetch` lädt den Content
- Nicht-Standard-Frontmatter-Felder (`auto_active`, `runtime`, `tags`) werden toleriert

**Warum es trotzdem nicht der richtige Weg ist:**

### 1. Skalierung — das Killer-Argument

Claude Code injiziert **alle Skill-Descriptions in jeden Turn** als System-Reminder. Bei 50 Skills sind das ~12.500 Zeichen. Mit 100 Caps dazu:

- ~37.500 Zeichen pro Turn → 18x über dem `skillListingBudgetFraction`-Budget (1% des Context-Windows)
- Massive Truncation der Descriptions
- Skill-Eval pro Turn: 150 Einträge evaluieren statt 50

Der `<caps-available>` Katalog im Proxy ist dafür gebaut: eine kompakte Tabelle (~1.5k für alle Caps), einmal im System-Block, nicht pro Skill gelistet. 100 Caps im Katalog = ~5k Tokens. 100 Caps als Skills = unbezahlbarer Overhead pro Turn.

### 2. Semantik — Caps sind keine Skills

Skills sind projektübergreifende Techniken (Commit-Standards, TDD-Workflow, Debugging-Methodik). Caps sind ausführbare Werkzeuge (Reddit fetchen, Daten analysieren, deployen). Diese Unterscheidung in einem Verzeichnis zu vermischen erzeugt:

- Rauschen in der Skill-Liste (Tools die keine Anleitungen sind)
- Verwirrung bei Skill-Eval (ist das eine Technik oder ein Tool?)
- Falsche Erwartungen bei Nutzern ("warum ist reddit_fetch ein Skill?")

### 3. Zukunftssicherheit — Caps werden wachsen

CAP.md kann Features aufnehmen die SKILL.md nicht unterstützt:

- **Versioning** — v1, v2, v3 mit Supersede-Ketten
- **Database-Section** — SQL-Schema für per-Cap-Tabellen
- **Dependencies** — `requires: [cap_store, blob_put]` mit Provider-Mapping
- **Testing-Metadata** — `tested: true`, `test_date`, Verifikationsstatus
- **Multi-Handler** — `handler_repl` + `handler_bash` in einer Datei

SKILL.md ist bewusst einfach gehalten (name + description im Frontmatter). Diese Einfachheit ist eine Stärke für Skills, aber eine Einschränkung für Caps.

### 4. Provider-Agnostische Adapter-Schicht

Caps nutzen generische Funktionen (`cap_store`, `blob_put`) statt provider-spezifische MCP-Tools. Jeder Provider (yesmem, andere) registriert seine eigenen Adapter:

```
Cap-Code:     await cap_store({capability: "mein_tool", action: "upsert", ...})
                     │
         ┌───────────┼───────────┐
         ▼           ▼           ▼
    yesmem-Adapter  Provider-X  Fallback
    → mcp__yesmem__ → mcp__x__  → in-memory
      cap_store       store       (session-only)
```

Dieses Muster erfordert ein Austauschformat das die `requires`-Dependencies deklariert — SKILL.md hat dafür keinen Standard-Mechanismus.

## Entscheidung

- **Austauschformat:** `CAP.md` in `~/.claude/caps/<name>/CAP.md`
- **Runtime-Persistenz:** yesmem DB (learnings, category='cap')
- **Discovery:** Proxy-injizierter `<caps-available>` Katalog, nicht Skill-Listing
- **Aktivierung:** `mcp__yesmem__activate_cap` MCP-Tool + `eval(result.code)` im REPL
- **Portabilität:** Generische Adapter-Schicht (`cap_store`, `blob_put`) mit Provider-Mapping

## Referenzen

- Design-Entscheidung: [ID:54523]
- CAP.md Format-Spezifikation: [ID:54333]
- Scanner-Design: [ID:54339]
- Skill-Listing-Overhead: [ID:52067]
- Adapter-Pattern-Diskussion: Session 2026-04-22
