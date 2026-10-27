/* shpellcast viewer.
 *
 * Two jobs:
 *   1. a small, honest terminal emulator — enough of VT100 that progress
 *      bars written with \r, spinners, and ANSI colour come out looking
 *      like they do in the shell, which is the whole point of watching a
 *      long-running job from a browser;
 *   2. an SSE client that survives reconnects without duplicating output.
 *
 * No dependencies, no build step, strict-CSP clean (no inline script,
 * no inline style attributes — colours are classes).
 */
(function () {
  'use strict';

  var MAX_LINES = 5000;   // scrollback kept in the DOM
  var MAX_COLS = 4000;    // guard against a single pathological line
  var POLL_MS = 8000;     // metadata refresh while a session is live

  var sid = document.body.getAttribute('data-session');
  var termEl = document.getElementById('term');
  var statusEl = document.getElementById('status');
  var statusText = document.getElementById('status-text');
  var statsEl = document.getElementById('stats');
  var followBtn = document.getElementById('follow');
  var wrapBtn = document.getElementById('wrap');
  var clearBtn = document.getElementById('clear');
  var copyBtn = document.getElementById('copy');

  /* ------------------------------------------------------------------ *
   * ANSI colour helpers
   * ------------------------------------------------------------------ */

  // Reference RGB for the 16 palette slots, used to fold 256-colour and
  // 24-bit sequences down to classes (inline styles would need an
  // 'unsafe-inline' CSP, which is not worth it for log output).
  var PALETTE = [
    [0, 0, 0], [205, 49, 49], [13, 188, 121], [229, 229, 16],
    [36, 114, 200], [188, 63, 188], [17, 168, 205], [229, 229, 229],
    [102, 102, 102], [241, 76, 76], [35, 209, 139], [245, 245, 67],
    [59, 142, 234], [214, 112, 214], [41, 184, 219], [255, 255, 255]
  ];

  function nearest16(r, g, b) {
    var best = 7, bestD = Infinity;
    for (var i = 0; i < 16; i++) {
      var p = PALETTE[i];
      var d = (p[0] - r) * (p[0] - r) + (p[1] - g) * (p[1] - g) + (p[2] - b) * (p[2] - b);
      if (d < bestD) { bestD = d; best = i; }
    }
    return best;
  }

  function cube256(n) {
    if (n < 16) return n;
    if (n < 232) {
      var i = n - 16;
      var steps = [0, 95, 135, 175, 215, 255];
      return nearest16(steps[Math.floor(i / 36) % 6], steps[Math.floor(i / 6) % 6], steps[i % 6]);
    }
    var v = 8 + (n - 232) * 10;
    return nearest16(v, v, v);
  }

  /* ------------------------------------------------------------------ *
   * Terminal
   * ------------------------------------------------------------------ */

  function Term(el) {
    this.el = el;
    this.reset();
  }

  Term.prototype.reset = function () {
    while (this.el.firstChild) this.el.removeChild(this.el.firstChild);
    this.lines = [];
    this.row = 0;
    this.col = 0;
    this.dirty = new Set();
    this.frame = 0;
    this.decoder = new TextDecoder('utf-8');
    this.state = 0;      // 0 normal, 1 esc, 2 csi, 3 osc, 4 osc-esc, 5 skip-one
    this.params = '';
    this.bytes = 0;
    this.sgrReset();
    this.newLine();
  };

  Term.prototype.sgrReset = function () {
    this.fg = -1; this.bg = -1;
    this.bold = false; this.dim = false; this.italic = false;
    this.under = false; this.strike = false; this.inverse = false;
    this.key = '';
  };

  Term.prototype.recomputeKey = function () {
    var fg = this.fg, bg = this.bg;
    if (this.inverse) { var t = fg; fg = bg === -1 ? 0 : bg; bg = t === -1 ? 7 : t; }
    var k = [];
    if (fg >= 0) k.push('f' + fg);
    if (bg >= 0) k.push('g' + bg);
    if (this.bold) k.push('b');
    if (this.dim) k.push('d');
    if (this.italic) k.push('i');
    if (this.under) k.push('u');
    if (this.strike) k.push('s');
    this.key = k.join(' ');
  };

  Term.prototype.newLine = function () {
    var el = document.createElement('div');
    el.className = 'ln';
    this.el.appendChild(el);
    var line = { cells: [], el: el, plain: true };
    this.lines.push(line);
    this.row = this.lines.length - 1;
    this.col = 0;

    // Trim scrollback from the front so memory and layout stay bounded
    // no matter how much a job prints.
    while (this.lines.length > MAX_LINES) {
      var gone = this.lines.shift();
      if (gone.el.parentNode) gone.el.parentNode.removeChild(gone.el);
      this.dirty.delete(gone);
      this.row--;
    }
    if (this.row < 0) this.row = 0;
    return line;
  };

  Term.prototype.cur = function () {
    if (!this.lines.length) return this.newLine();
    if (this.row >= this.lines.length) this.row = this.lines.length - 1;
    if (this.row < 0) this.row = 0;
    return this.lines[this.row];
  };

  Term.prototype.markDirty = function (line) {
    this.dirty.add(line);
    this.schedule();
  };

  Term.prototype.put = function (ch) {
    if (this.col >= MAX_COLS) return;
    var line = this.cur();
    var cells = line.cells;
    while (cells.length < this.col) cells.push({ c: ' ', k: '' });
    cells[this.col] = { c: ch, k: this.key };
    if (this.key) line.plain = false;
    this.col++;
    this.markDirty(line);
  };

  Term.prototype.lineFeed = function () {
    if (this.row === this.lines.length - 1) {
      this.newLine();
    } else {
      this.row++;
      this.col = 0;
    }
  };

  Term.prototype.eraseInLine = function (mode) {
    var line = this.cur();
    if (mode === 1) {
      for (var i = 0; i <= this.col && i < line.cells.length; i++) line.cells[i] = { c: ' ', k: '' };
    } else if (mode === 2) {
      line.cells = [];
    } else {
      line.cells.length = Math.min(line.cells.length, this.col);
    }
    this.markDirty(line);
  };

  Term.prototype.clearScreen = function () {
    while (this.el.firstChild) this.el.removeChild(this.el.firstChild);
    this.lines = [];
    this.dirty.clear();
    this.newLine();
  };

  Term.prototype.csi = function (final) {
    var raw = this.params.replace(/^[?>!]/, '');
    var parts = raw.length ? raw.split(';') : [];
    var n = function (i, d) {
      var v = parseInt(parts[i], 10);
      return isNaN(v) || v === 0 ? (d === undefined ? 1 : d) : v;
    };

    switch (final) {
      case 'm': this.sgr(parts); break;
      case 'K': this.eraseInLine(parseInt(parts[0], 10) || 0); break;
      case 'J': {
        var m = parseInt(parts[0], 10) || 0;
        if (m === 2 || m === 3) this.clearScreen();
        else if (m === 0) {
          this.lines.splice(this.row + 1).forEach(function (l) {
            if (l.el.parentNode) l.el.parentNode.removeChild(l.el);
          });
          this.eraseInLine(0);
        }
        break;
      }
      case 'A': this.row = Math.max(0, this.row - n(0)); break;
      case 'B': this.row = Math.min(this.lines.length - 1, this.row + n(0)); break;
      case 'C': this.col = Math.min(MAX_COLS, this.col + n(0)); break;
      case 'D': this.col = Math.max(0, this.col - n(0)); break;
      case 'E': this.row = Math.min(this.lines.length - 1, this.row + n(0)); this.col = 0; break;
      case 'F': this.row = Math.max(0, this.row - n(0)); this.col = 0; break;
      case 'G': case '`': this.col = Math.max(0, n(0) - 1); break;
      case 'd': this.row = Math.max(0, Math.min(this.lines.length - 1, n(0) - 1)); break;
      case 'H': case 'f':
        // No fixed viewport here (the page scrolls), so absolute
        // positioning is interpreted relative to the current scrollback.
        this.row = Math.max(0, Math.min(this.lines.length - 1, n(0) - 1));
        this.col = Math.max(0, n(1) - 1);
        break;
      default: break; // modes, scroll regions, cursor save/restore: ignored
    }
  };

  Term.prototype.sgr = function (parts) {
    if (!parts.length) parts = ['0'];
    for (var i = 0; i < parts.length; i++) {
      var p = parseInt(parts[i], 10);
      if (isNaN(p)) p = 0;
      if (p === 0) { this.sgrReset(); continue; }
      else if (p === 1) this.bold = true;
      else if (p === 2) this.dim = true;
      else if (p === 3) this.italic = true;
      else if (p === 4) this.under = true;
      else if (p === 7) this.inverse = true;
      else if (p === 9) this.strike = true;
      else if (p === 22) { this.bold = false; this.dim = false; }
      else if (p === 23) this.italic = false;
      else if (p === 24) this.under = false;
      else if (p === 27) this.inverse = false;
      else if (p === 29) this.strike = false;
      else if (p >= 30 && p <= 37) this.fg = p - 30;
      else if (p === 39) this.fg = -1;
      else if (p >= 40 && p <= 47) this.bg = p - 40;
      else if (p === 49) this.bg = -1;
      else if (p >= 90 && p <= 97) this.fg = p - 90 + 8;
      else if (p >= 100 && p <= 107) this.bg = p - 100 + 8;
      else if (p === 38 || p === 48) {
        var mode = parseInt(parts[i + 1], 10);
        var val = -1;
        if (mode === 5) { val = cube256(parseInt(parts[i + 2], 10) || 0); i += 2; }
        else if (mode === 2) {
          val = nearest16(parseInt(parts[i + 2], 10) || 0, parseInt(parts[i + 3], 10) || 0, parseInt(parts[i + 4], 10) || 0);
          i += 4;
        }
        if (val >= 0) { if (p === 38) this.fg = val; else this.bg = val; }
      }
    }
    this.recomputeKey();
  };

  Term.prototype.feedText = function (str) {
    for (var idx = 0; idx < str.length; idx++) {
      var ch = str[idx];
      // Keep surrogate pairs together so emoji and CJK survive.
      var code = str.charCodeAt(idx);
      if (code >= 0xd800 && code <= 0xdbff && idx + 1 < str.length) {
        ch = str.substr(idx, 2); idx++;
      }

      switch (this.state) {
        case 0:
          if (ch === '\x1b') { this.state = 1; }
          else if (ch === '\n') { this.lineFeed(); }
          else if (ch === '\r') { this.col = 0; }
          else if (ch === '\b') { this.col = Math.max(0, this.col - 1); }
          else if (ch === '\t') { var stop = (Math.floor(this.col / 8) + 1) * 8; while (this.col < stop) this.put(' '); }
          else if (ch === '\x0c') { this.clearScreen(); }
          else if (ch < ' ' || ch === '\x7f') { /* ignore other controls */ }
          else { this.put(ch); }
          break;

        case 1:
          if (ch === '[') { this.state = 2; this.params = ''; }
          else if (ch === ']') { this.state = 3; }
          else if (ch === '(' || ch === ')' || ch === '#') { this.state = 5; }
          else if (ch === 'c') { this.sgrReset(); this.clearScreen(); this.state = 0; }
          else { this.state = 0; }
          break;

        case 2:
          if (ch >= '\x20' && ch <= '\x3f') {
            if (this.params.length < 64) this.params += ch;
          } else {
            this.csi(ch);
            this.state = 0;
          }
          break;

        case 3: // OSC: window titles, hyperlinks — consumed and dropped
          if (ch === '\x07') this.state = 0;
          else if (ch === '\x1b') this.state = 4;
          break;

        case 4:
          this.state = 0;
          break;

        case 5:
          this.state = 0;
          break;
      }
    }
  };

  Term.prototype.feed = function (bytes) {
    this.bytes += bytes.length;
    this.feedText(this.decoder.decode(bytes, { stream: true }));
  };

  Term.prototype.note = function (text, warn) {
    var el = document.createElement('div');
    el.className = warn ? 'note warn' : 'note';
    el.textContent = text;
    this.el.appendChild(el);
    this.lines.push({ cells: [], el: el, plain: true, isNote: true });
    this.row = this.lines.length - 1;
    this.col = 0;
    this.schedule();
  };

  Term.prototype.schedule = function () {
    if (this.frame) return;
    var self = this;
    this.frame = requestAnimationFrame(function () {
      self.frame = 0;
      self.render();
    });
  };

  Term.prototype.render = function () {
    var stick = follow && atBottom();
    this.dirty.forEach(function (line) {
      if (line.isNote) return;
      renderLine(line);
    });
    this.dirty.clear();
    if (stick) scrollToBottom();
  };

  function renderLine(line) {
    var cells = line.cells;
    // Fast path: an uncoloured line is a single text node.
    if (line.plain) {
      var s = '';
      for (var i = 0; i < cells.length; i++) s += cells[i].c;
      line.el.textContent = s;
      return;
    }
    var frag = document.createDocumentFragment();
    var runKey = null, runText = '';
    function flush() {
      if (runText === '') return;
      if (runKey) {
        var span = document.createElement('span');
        span.className = runKey;
        span.textContent = runText;
        frag.appendChild(span);
      } else {
        frag.appendChild(document.createTextNode(runText));
      }
      runText = '';
    }
    for (var j = 0; j < cells.length; j++) {
      var c = cells[j];
      if (c.k !== runKey) { flush(); runKey = c.k; }
      runText += c.c;
    }
    flush();
    while (line.el.firstChild) line.el.removeChild(line.el.firstChild);
    line.el.appendChild(frag);
  }

  /* ------------------------------------------------------------------ *
   * Scrolling / controls
   * ------------------------------------------------------------------ */

  var follow = true;
  var selfScroll = false;

  function atBottom() {
    return termEl.scrollHeight - termEl.scrollTop - termEl.clientHeight < 40;
  }
  function scrollToBottom() {
    selfScroll = true;
    termEl.scrollTop = termEl.scrollHeight;
    requestAnimationFrame(function () { selfScroll = false; });
  }
  function setFollow(on) {
    follow = on;
    followBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
    if (on) scrollToBottom();
  }

  termEl.addEventListener('scroll', function () {
    if (selfScroll) return;
    var bottom = atBottom();
    if (!bottom && follow) setFollow(false);
    else if (bottom && !follow) setFollow(true);
  }, { passive: true });

  followBtn.addEventListener('click', function () { setFollow(!follow); });

  function store(key, val) {
    try { localStorage.setItem(key, val); } catch (e) { /* private mode */ }
  }
  function load(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }

  function setWrap(on) {
    termEl.classList.toggle('wrap', on);
    wrapBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
    store('shpellcast.wrap', on ? '1' : '0');
  }
  wrapBtn.addEventListener('click', function () {
    setWrap(!termEl.classList.contains('wrap'));
  });
  setWrap(load('shpellcast.wrap') === '1');

  clearBtn.addEventListener('click', function () {
    term.clearScreen();
    term.note('display cleared locally — reload to replay the buffer');
  });

  copyBtn.addEventListener('click', function () {
    var url = window.location.href;
    var done = function () {
      var old = copyBtn.textContent;
      copyBtn.textContent = 'Copied';
      setTimeout(function () { copyBtn.textContent = old; }, 1200);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(url).then(done, function () { window.prompt('Copy this URL', url); });
    } else {
      window.prompt('Copy this URL', url);
    }
  });

  /* ------------------------------------------------------------------ *
   * Status + stats
   * ------------------------------------------------------------------ */

  var meta = null;
  var startedAt = null;

  function setStatus(state, text) {
    statusEl.setAttribute('data-state', state);
    statusText.textContent = text || state;
  }

  function human(n) {
    if (n < 1024) return n + ' B';
    var units = ['KB', 'MB', 'GB', 'TB'], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
    return n.toFixed(1) + ' ' + units[i];
  }

  function duration(ms) {
    var s = Math.max(0, Math.floor(ms / 1000));
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
    if (h) return h + 'h ' + m + 'm';
    if (m) return m + 'm ' + sec + 's';
    return sec + 's';
  }

  function renderStats() {
    if (!meta) return;
    var bits = [human(meta.bytes || 0)];
    var end = meta.closed && meta.closed_at ? new Date(meta.closed_at) : new Date();
    if (startedAt) bits.push(duration(end - startedAt));
    if (meta.live_viewers > 1) bits.push(meta.live_viewers + ' viewers');
    if (meta.truncated) bits.push('truncated');
    statsEl.textContent = bits.join(' · ');
  }

  function applyMeta(m) {
    meta = m;
    startedAt = new Date(m.created_at);
    if (m.truncated && !applyMeta.warned) {
      applyMeta.warned = true;
    }
    renderStats();
  }

  function pollMeta() {
    if (meta && meta.closed) return;
    fetch('/s/' + sid + '/meta', { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (m) { if (m) { applyMeta(m); } })
      .catch(function () { /* transient */ });
  }
  setInterval(pollMeta, POLL_MS);
  setInterval(renderStats, 1000);

  /* ------------------------------------------------------------------ *
   * Transport
   * ------------------------------------------------------------------ */

  var term = new Term(termEl);
  var es = null;
  var retry = 0;

  function b64ToBytes(s) {
    var bin = atob(s);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function connect() {
    setStatus('connecting', 'connecting');
    es = new EventSource('/s/' + sid + '/stream');

    es.addEventListener('meta', function (e) {
      retry = 0;
      // Every connection starts with meta and then replays the retained
      // buffer, so resetting here makes reconnects idempotent instead of
      // duplicating output.
      term.reset();
      try { applyMeta(JSON.parse(e.data)); } catch (err) { /* ignore */ }
      if (meta && meta.truncated) {
        term.note('earlier output was dropped — this buffer keeps only the most recent bytes', true);
      }
      setStatus(meta && meta.closed ? 'closed' : 'live', meta && meta.closed ? 'finished' : 'live');
      scrollToBottom();
    });

    es.addEventListener('out', function (e) {
      term.feed(b64ToBytes(e.data));
    });

    es.addEventListener('gap', function () {
      term.note('viewer fell behind; reconnecting and replaying', true);
    });

    es.addEventListener('end', function () {
      setStatus('closed', 'finished');
      if (es) { es.close(); es = null; }
      pollMeta();
    });

    es.onerror = function () {
      if (!es) return;
      if (es.readyState === EventSource.CLOSED) {
        // The browser gave up; back off and try again ourselves.
        es = null;
        retry = Math.min(retry + 1, 6);
        setStatus('error', 'reconnecting');
        setTimeout(connect, Math.min(1000 * Math.pow(2, retry), 30000));
      } else {
        setStatus('connecting', 'reconnecting');
      }
    };
  }

  document.title = 'shpellcast · ' + sid;
  setFollow(true);
  connect();
  pollMeta();
})();
