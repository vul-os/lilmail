// assets/js/app.js — LilMail shell: Alpine stores, HTMX wiring, keyboard nav.
//
// Loaded on every page (signed in or not). Originally an inline <script> in
// main.html; ⚠️ NOT extractable verbatim — the htmx:configRequest handler
// read the bearer token via a Go template interpolation, `const token =
// '{{.Token}}';`, which cannot survive in a static file. It now reads the
// same value from the '#app-token' DOM element's data-token attribute
// instead (rendered by the server as <span id="app-token"
// data-token="{{.Token}}"> — see templates/layouts/main.html), which is
// present under the identical {{if .Token}} condition the inline literal
// depended on, so behaviour is unchanged: empty string when there is no
// token, the same token value when there is one.
'use strict';

document.addEventListener('alpine:init', function () {
    Alpine.store('ui', { sidebarOpen: false });

    // ---- mail layout / preferences store (persisted) ----
    /**
     * @param {string} key
     * @param {string} fallback
     * @returns {string}
     */
    function loadPref(key, fallback) {
        try {
            const v = localStorage.getItem('lm.' + key);
            return v === null ? fallback : v;
        } catch {
            return fallback;
        }
    }
    Alpine.store('mail', {
        // reading-pane position: 'right' | 'bottom' | 'off'
        pane: loadPref('pane', 'right'),
        // list density: 'comfortable' | 'compact'
        density: loadPref('density', 'comfortable'),
        selection: /** @type {string[]} */ ([]), // selected email IDs (multi-select)
        activeId: /** @type {string | null} */ (null), // currently-open message id (for keyboard nav)
        /** @param {string} p */
        setPane: function (p) {
            this.pane = p;
            try { localStorage.setItem('lm.pane', p); } catch { /* storage unavailable */ }
        },
        toggleDensity: function () {
            this.density = this.density === 'compact' ? 'comfortable' : 'compact';
            try { localStorage.setItem('lm.density', this.density); } catch { /* storage unavailable */ }
        },
        /** @param {string} id */
        isSelected: function (id) { return this.selection.indexOf(id) !== -1; },
        /** @param {string} id */
        toggleSelect: function (id) {
            const i = this.selection.indexOf(id);
            if (i === -1) this.selection.push(id); else this.selection.splice(i, 1);
        },
        clearSelection: function () { this.selection = []; }
    });
});

// Reflect layout prefs as classes on <body> so CSS can react globally.
document.addEventListener('alpine:initialized', function () {
    const s = Alpine.store('mail');
    function sync() {
        document.body.dataset.pane = s.pane;
        document.body.dataset.density = s.density;
    }
    sync();
    // Alpine effects re-run sync when store props change.
    Alpine.effect(sync);
});

// CSRF double-submit token from the "_csrf" cookie. htmx requests get it below;
// raw fetch() calls that POST/PUT/DELETE must send it as X-CSRF-Token themselves
// (without it the CSRF middleware answers 403 Forbidden).
/** @type {any} */ (window).lmCsrfToken = function () {
    const m = document.cookie.match(/(?:^|;\s*)_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : '';
};

document.body.addEventListener('htmx:configRequest', function (evt) {
    const tokenEl = document.getElementById('app-token');
    const token = tokenEl instanceof HTMLElement && tokenEl.dataset.token ? tokenEl.dataset.token : '';
    if (token) evt.detail.headers['Authorization'] = 'Bearer ' + token;
    // Double-submit CSRF: read the "_csrf" cookie set by the server and
    // echo it back as a header so the CSRF middleware can validate it.
    const csrfMatch = document.cookie.match(/(?:^|;\s*)_csrf=([^;]+)/);
    if (csrfMatch) evt.detail.headers['X-CSRF-Token'] = decodeURIComponent(csrfMatch[1]);
});

(function () {
    const bar = document.getElementById('htmx-loading-bar');
    if (!(bar instanceof HTMLElement)) return;
    const loadingBar = bar;
    document.addEventListener('htmx:beforeRequest', function () { loadingBar.style.width = '50%'; });
    document.addEventListener('htmx:afterRequest', function () {
        loadingBar.style.width = '100%';
        setTimeout(function () { loadingBar.style.width = '0'; }, 300);
    });
})();

// ---- Keyboard navigation (Thunderbird-style) ----
// j/k move between message rows, Enter/o opens, r replies, u marks unread,
// Delete/Backspace deletes, c composes, / focuses search, Esc closes viewer.
(function () {
    /**
     * @param {Element | null} el
     * @returns {boolean}
     */
    function isTyping(el) {
        if (!el) return false;
        const t = el.tagName;
        return t === 'INPUT' || t === 'TEXTAREA' || t === 'SELECT' || /** @type {HTMLElement} */ (el).isContentEditable;
    }
    /** @returns {HTMLElement[]} */
    function rows() {
        return Array.from(document.querySelectorAll('#email-list-inner .email-row'))
            .filter(function (el) { return el instanceof HTMLElement; });
    }
    /**
     * @param {HTMLElement[]} list
     * @returns {number}
     */
    function currentIndex(list) {
        for (let i = 0; i < list.length; i++) {
            if (list[i].classList.contains('is-cursor')) return i;
        }
        return -1;
    }
    /**
     * @param {HTMLElement[]} list
     * @param {number} idx
     */
    function focusRow(list, idx) {
        list.forEach(function (r) { r.classList.remove('is-cursor'); });
        if (idx < 0 || idx >= list.length) return;
        const r = list[idx];
        r.classList.add('is-cursor');
        r.scrollIntoView({ block: 'nearest' });
    }
    document.addEventListener('keydown', function (e) {
        if (e.metaKey || e.ctrlKey || e.altKey) return;
        if (isTyping(document.activeElement)) {
            if (e.key === 'Escape' && document.activeElement instanceof HTMLElement) document.activeElement.blur();
            return;
        }
        /** @type {HTMLElement[]} */
        let list;
        /** @type {number} */
        let idx;
        switch (e.key) {
            case 'j': case 'ArrowDown':
                list = rows(); if (!list.length) return;
                idx = currentIndex(list);
                focusRow(list, Math.min(idx + 1, list.length - 1));
                e.preventDefault();
                break;
            case 'k': case 'ArrowUp':
                list = rows(); if (!list.length) return;
                idx = currentIndex(list);
                focusRow(list, idx <= 0 ? 0 : idx - 1);
                e.preventDefault();
                break;
            case 'Enter': case 'o':
                list = rows(); idx = currentIndex(list);
                if (idx >= 0) { list[idx].click(); e.preventDefault(); }
                break;
            case 'c': {
                const cb = document.querySelector('.btn-compose');
                if (cb instanceof HTMLElement) { cb.click(); e.preventDefault(); }
                break;
            }
            case 'r': {
                const rb = document.querySelector('#email-viewer-pane [data-action="reply"]');
                if (rb instanceof HTMLElement) { rb.click(); e.preventDefault(); }
                break;
            }
            case 'a': {
                const ab = document.querySelector('#email-viewer-pane [data-action="archive"]');
                if (ab instanceof HTMLElement) { ab.click(); e.preventDefault(); }
                break;
            }
            case 'u': {
                const ub = document.querySelector('#email-viewer-pane [data-action="unread"]');
                if (ub instanceof HTMLElement) { ub.click(); e.preventDefault(); }
                break;
            }
            case 'Delete': case 'Backspace': case '#': {
                const db = document.querySelector('#email-viewer-pane [data-action="delete"]');
                if (db instanceof HTMLElement) { db.click(); e.preventDefault(); }
                break;
            }
            case '/': {
                const si = document.querySelector('.topbar__search-input');
                if (si instanceof HTMLElement) { si.focus(); e.preventDefault(); }
                break;
            }
        }
    });
})();
