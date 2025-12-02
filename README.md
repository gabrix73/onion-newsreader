# 🧅 Onion Newsreader

A minimal, privacy-focused NNTP client that operates entirely within the Tor network.

## Overview

Onion Newsreader is a web-based Usenet client designed for anonymous access. The entire data path stays on Tor - from reading articles to posting replies, no traffic ever touches the clearnet.

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Onion Reader   │────▶│    m2usenet     │────▶│   SMTP Relay    │
│    (.onion)     │     │    (.onion)     │     │    (.onion)     │
└─────────────────┘     └─────────────────┘     └─────────────────┘
                                                        │
                                                        ▼
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   NNTP Server   │◀────│   Mail2News     │◀────┘
│    (.onion)     │     │    (.onion)     │
└─────────────────┘     └─────────────────┘
```

## Features

### 🧅 Full Tor Operation
- Connects to NNTP servers via SOCKS5 proxy (127.0.0.1:9050)
- Primary server: Tor hidden service (.onion)
- Fallback server: Clearnet via Tor exit node
- All HTTP traffic stays local (127.0.0.1:8080)

### 🧵 Threaded View
- Articles organized by thread hierarchy
- Visual indentation with colored borders per depth level
- Parent-child relationships built from References header
- Root threads (depth 0) clearly distinguished from replies

### 📝 Quote Reply
- Click Reply on any article to compose a response
- Automatic quote formatting: `On [date], [author] wrote:`
- Original message quoted with `>` prefix
- Signature stripped from quotes
- Pre-fills newsgroup, subject, and references

### ⭐ Favorite Newsgroups
- Star any newsgroup for quick access
- Dedicated Favorites page
- Persistent storage in `favorites.json`
- One-click access to post or browse

### 🔔 Thread Notifications
- Watch any thread starter for new replies
- Bell icon on root posts (depth 0) to toggle watching
- Manual check for new replies across all watched threads
- Badge shows count of new responses
- Persistent storage in `watched.json`

### 🔗 m2usenet Integration
- Seamless posting via m2usenet gateway
- Pre-filled fields: newsgroups, subject, references, quoted body
- Supports hashcash proof-of-work
- Ed25519 signature for sender verification
- Cross-origin redirect with all reply data

### 🔄 Robust Connectivity
- Automatic reconnection on Tor circuit changes
- 3 retry attempts for active file download
- Extended timeouts for .onion connections (180s)
- Graceful fallback to secondary server
- Connection keepalive with periodic checks

### 💾 Local Persistence
- `favorites.json` - Starred newsgroups
- `watched.json` - Watched threads with notification state
- No external database required
- Human-readable JSON format

## Installation

### Requirements
- Go 1.19+
- Tor running with SOCKS5 proxy on 127.0.0.1:9050

### Build

```bash
CGO_ENABLED=0 go build -o onion-newsreader onion-newsreader.go
```

### Run

```bash
./onion-newsreader
```

The web interface will be available at `http://127.0.0.1:8080`

### Systemd Service (optional)

```ini
[Unit]
Description=Onion Newsreader
After=tor.service
Requires=tor.service

[Service]
Type=simple
User=newsreader
WorkingDirectory=/opt/onion-newsreader
ExecStart=/opt/onion-newsreader/bin/onion-newsreader
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## Usage

### First Run

1. Open `http://127.0.0.1:8080` in Tor Browser
2. Click **📥 Download Active File** to fetch newsgroup list
3. Wait 2-10 minutes (large file over Tor)
4. Use **🔍 Search** to find groups (wildcards: `comp.*`, `*linux*`)

### Reading

1. Click a group name to view articles
2. Articles displayed in threaded view
3. Click article subject to read full content
4. Indentation shows reply hierarchy

### Posting

1. Click **📝 New Post** or **💬 Reply**
2. m2usenet opens with pre-filled fields
3. Generate hashcash token (proof-of-work)
4. Sign message with Ed25519 key
5. Submit to post via Tor

### Favorites

1. Open any group
2. Click **☆** next to group name to add favorite
3. Access favorites via **⭐ Favorites** in navigation
4. Click **★** to remove from favorites

### Notifications

1. In group view, click **🔕** on a thread starter
2. Thread shows **🔔** when watched
3. Go to **🔔 Notifications** page
4. Click **🔄 Check for new replies**
5. Badge shows new reply count
6. Click **✓ Mark read** to clear

## Configuration

Edit constants in source code:

```go
const (
    M2UsenetURL   = "http://[your-m2usenet].onion:8880"
    TorProxyAddr  = "127.0.0.1:9050"
    PrimaryNNTP   = "[your-nntp].onion:119"
    FallbackNNTP  = "news.example.net:119"
    FavoritesFile = "favorites.json"
    WatchedFile   = "watched.json"
)
```

## NNTP Server Compatibility

Tested with:
- INN (InterNetNews)
- Leafnode
- Any RFC 3977 compliant server

Required commands:
- `LIST` - Active file download
- `GROUP` - Select newsgroup
- `XOVER` - Article overview (subject, from, date, message-id, references)
- `ARTICLE` - Full article retrieval

## Tor Browser Notes

### NoScript Warning

When clicking Reply, NoScript may show an XSS warning due to the cross-origin redirect with quoted text in URL parameters. This is a **false positive** - the data is just the quoted message for the reply form.

Solutions:
- Click "Temporarily allow"
- Add both .onion addresses to NoScript whitelist

### Recommended Settings

- Security Level: Standard or Safer
- JavaScript: Enabled (required for hashcash mining)
- Cookies: Allow for session

## File Structure

```
onion-newsreader/
├── onion-newsreader.go    # Single-file source
├── onion-newsreader       # Compiled binary
├── favorites.json         # Saved favorite groups
├── watched.json           # Watched threads state
└── README.md
```

## Technical Details

### Threading Algorithm

1. Parse XOVER response to extract References header
2. Build map of Message-ID → Article
3. Extract parent ID (last Message-ID in References)
4. Link children to parents
5. Orphaned replies (parent not in set) become roots
6. Flatten tree with depth annotation
7. Sort: roots by date desc, children by date asc

### Notification Check

1. Group watched posts by newsgroup
2. Fetch recent articles (XOVER) for each group
3. For each article, check if References contains watched Message-ID
4. Increment reply counter for matches
5. Update last-checked timestamp

### Security Model

- No authentication stored (stateless)
- No cookies required
- No external requests except Tor SOCKS5
- Local-only HTTP binding
- JSON files readable only by owner

## License

MIT License

## Links

- **Newsreader**: `http://qgaswy4ebtrhaargqvoboutky7xoyyx5rq5nhydixemkniresdze5dyd.onion:8043`
- **m2usenet**: `http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880`
- **Source**: `https://github.com/gabrix73/onion-newsreader`

---

*Because Usenet deserves privacy too.* 🧅📰
