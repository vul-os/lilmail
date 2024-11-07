// static/js/app.js
class WebmailApp {
    constructor() {
        this.emailClient = new EmailClient();
        this.ui = new UIManager();
        this.cache = new CacheManager();
        
        // Initialize based on current page
        const page = document.body.dataset.page;
        switch (page) {
            case 'inbox':
                new InboxPage(this);
                break;
            case 'message':
                new MessagePage(this);
                break;
            case 'compose':
                new ComposePage(this);
                break;
        }

        // Global event listeners
        this.setupGlobalListeners();
    }

    setupGlobalListeners() {
        // Handle session timeout
        document.addEventListener('fetch-error', (e) => {
            if (e.detail.status === 401) {
                this.ui.showAlert('Session expired. Please login again.', 'error');
                window.location.href = '/login';
            }
        });

        // Handle offline status
        window.addEventListener('online', () => {
            this.ui.hideAlert('offline');
            this.emailClient.sync();
        });

        window.addEventListener('offline', () => {
            this.ui.showAlert('You are offline. Some features may be unavailable.', 'warning', 'offline');
        });
    }
}

// static/js/email-client.js
class EmailClient {
    constructor() {
        this.baseUrl = '/api';
        this.pendingRequests = new Map();
    }

    async request(endpoint, options = {}) {
        const requestId = Math.random().toString(36).substring(7);
        this.pendingRequests.set(requestId, { endpoint, options });

        try {
            const response = await fetch(`${this.baseUrl}${endpoint}`, {
                ...options,
                headers: {
                    'Content-Type': 'application/json',
                    ...options.headers,
                }
            });

            if (!response.ok) {
                throw new Error(`HTTP error! status: ${response.status}`);
            }

            const data = await response.json();
            return data;
        } catch (error) {
            document.dispatchEvent(new CustomEvent('fetch-error', {
                detail: { status: error.response?.status, error }
            }));
            throw error;
        } finally {
            this.pendingRequests.delete(requestId);
        }
    }

    async getFolders() {
        return this.request('/folders');
    }

    async getMessages(folder, page = 1, limit = 50) {
        return this.request(`/folder/${folder}?page=${page}&limit=${limit}`);
    }

    async getMessage(folder, uid) {
        return this.request(`/folder/${folder}/message/${uid}`);
    }

    async moveMessage(folder, uid, destination) {
        return this.request(`/folder/${folder}/message/${uid}/move`, {
            method: 'POST',
            body: JSON.stringify({ destination })
        });
    }

    async deleteMessage(folder, uid) {
        return this.request(`/folder/${folder}/message/${uid}`, {
            method: 'DELETE'
        });
    }

    async sync() {
        return this.request('/sync', { method: 'POST' });
    }
}

// static/js/ui-manager.js
class UIManager {
    constructor() {
        this.alerts = new Set();
        this.setupTheme();
    }

    setupTheme() {
        const theme = localStorage.getItem('theme') || 'light';
        document.documentElement.dataset.theme = theme;
        
        const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
        mediaQuery.addEventListener('change', (e) => {
            if (!localStorage.getItem('theme')) {
                document.documentElement.dataset.theme = e.matches ? 'dark' : 'light';
            }
        });
    }

    showAlert(message, type = 'info', id = null) {
        const alertElement = document.createElement('div');
        alertElement.className = `alert alert-${type}`;
        alertElement.textContent = message;
        
        if (id) {
            alertElement.dataset.id = id;
            if (this.alerts.has(id)) {
                return;
            }
            this.alerts.add(id);
        }

        const alertContainer = document.querySelector('.alert-container') || 
            this.createAlertContainer();
        
        alertContainer.appendChild(alertElement);

        if (!id) {
            setTimeout(() => alertElement.remove(), 5000);
        }
    }

    hideAlert(id) {
        if (id) {
            const alert = document.querySelector(`.alert[data-id="${id}"]`);
            if (alert) {
                alert.remove();
                this.alerts.delete(id);
            }
        }
    }

    createAlertContainer() {
        const container = document.createElement('div');
        container.className = 'alert-container fixed top-4 right-4 z-50 space-y-2';
        document.body.appendChild(container);
        return container;
    }

    setLoading(element, isLoading) {
        if (isLoading) {
            element.classList.add('loading');
            element.disabled = true;
        } else {
            element.classList.remove('loading');
            element.disabled = false;
        }
    }
}

// static/js/cache-manager.js
class CacheManager {
    constructor() {
        this.store = new Map();
    }

    async get(key) {
        const cached = this.store.get(key);
        if (cached && Date.now() < cached.expires) {
            return cached.data;
        }
        return null;
    }

    set(key, data, ttl = 300000) { // 5 minutes default TTL
        this.store.set(key, {
            data,
            expires: Date.now() + ttl
        });
    }

    clear() {
        this.store.clear();
    }
}

// static/js/pages/inbox.js
class InboxPage {
    constructor(app) {
        this.app = app;
        this.currentFolder = document.querySelector('[data-current-folder]')?.dataset.currentFolder;
        this.setupListeners();
        this.setupInfiniteScroll();
    }

    setupListeners() {
        document.querySelectorAll('.message-list-item').forEach(item => {
            item.addEventListener('click', this.handleMessageClick.bind(this));
        });

        const refreshButton = document.querySelector('#refresh-button');
        if (refreshButton) {
            refreshButton.addEventListener('click', this.handleRefresh.bind(this));
        }
    }

    setupInfiniteScroll() {
        const messageList = document.querySelector('.message-list');
        if (!messageList) return;

        const observer = new IntersectionObserver(
            (entries) => {
                if (entries[0].isIntersecting) {
                    this.loadMoreMessages();
                }
            },
            { threshold: 0.5 }
        );

        const sentinel = document.createElement('div');
        sentinel.className = 'h-4';
        messageList.appendChild(sentinel);
        observer.observe(sentinel);
    }

    async loadMoreMessages() {
        if (this.isLoading || !this.hasMore) return;
        this.isLoading = true;

        try {
            const messages = await this.app.emailClient.getMessages(
                this.currentFolder,
                this.currentPage + 1
            );
            this.appendMessages(messages);
            this.currentPage++;
            this.hasMore = messages.length === this.pageSize;
        } catch (error) {
            this.app.ui.showAlert('Failed to load more messages', 'error');
        } finally {
            this.isLoading = false;
        }
    }

    appendMessages(messages) {
        const template = document.querySelector('#message-template');
        const container = document.querySelector('.message-list');

        messages.forEach(message => {
            const clone = template.content.cloneNode(true);
            // Populate template with message data
            clone.querySelector('.subject').textContent = message.subject;
            clone.querySelector('.from').textContent = message.from;
            clone.querySelector('.date').textContent = new Date(message.date).toLocaleString();
            
            container.appendChild(clone);
        });
    }

    async handleRefresh() {
        const button = document.querySelector('#refresh-button');
        this.app.ui.setLoading(button, true);

        try {
            await this.app.emailClient.sync();
            window.location.reload();
        } catch (error) {
            this.app.ui.showAlert('Failed to refresh messages', 'error');
        } finally {
            this.app.ui.setLoading(button, false);
        }
    }

    handleMessageClick(e) {
        const { messageUid, folder } = e.currentTarget.dataset;
        window.location.href = `/folder/${folder}/message/${messageUid}`;
    }
}

// static/js/pages/message.js
class MessagePage {
    constructor(app) {
        this.app = app;
        this.setupListeners();
        this.setupMessageContent();
    }

    setupListeners() {
        document.querySelector('#delete-button')?.addEventListener(
            'click', this.handleDelete.bind(this)
        );
        document.querySelector('#move-button')?.addEventListener(
            'click', this.handleMove.bind(this)
        );
    }

    setupMessageContent() {
        // Handle email content security
        const content = document.querySelector('.message-content');
        if (content) {
            this.sanitizeMessageContent(content);
        }

        // Setup attachment downloads
        document.querySelectorAll('.attachment-link').forEach(link => {
            link.addEventListener('click', this.handleAttachmentClick.bind(this));
        });
    }

    sanitizeMessageContent(element) {
        // Remove potentially dangerous elements and attributes
        const dangerous = ['script', 'iframe', 'object', 'embed', 'form'];
        dangerous.forEach(tag => {
            element.querySelectorAll(tag).forEach(el => el.remove());
        });

        // Remove event handlers
        element.querySelectorAll('*').forEach(el => {
            for (const attr of el.attributes) {
                if (attr.name.startsWith('on')) {
                    el.removeAttribute(attr.name);
                }
            }
        });

        // Handle images
        element.querySelectorAll('img').forEach(img => {
            if (!img.src.startsWith('data:') && !img.src.startsWith(window.location.origin)) {
                img.classList.add('remote-image');
                img.dataset.src = img.src;
                img.src = '/static/img/placeholder.png';
                img.addEventListener('click', this.handleImageLoad.bind(this));
            }
        });
    }

    handleImageLoad(e) {
        const img = e.currentTarget;
        if (confirm('Load remote image?')) {
            img.src = img.dataset.src;
            img.classList.remove('remote-image');
        }
    }

    async handleDelete() {
        if (!confirm('Are you sure you want to delete this message?')) {
            return;
        }

        const { folder, uid } = document.querySelector('[data-message]').dataset;
        
        try {
            await this.app.emailClient.deleteMessage(folder, uid);
            window.location.href = `/folder/${folder}`;
        } catch (error) {
            this.app.ui.showAlert('Failed to delete message', 'error');
        }
    }

    async handleMove() {
        const folder = prompt('Enter destination folder:');
        if (!folder) return;

        const { currentFolder, uid } = document.querySelector('[data-message]').dataset;
        
        try {
            await this.app.emailClient.moveMessage(currentFolder, uid, folder);
            window.location.href = `/folder/${currentFolder}`;
        } catch (error) {
            this.app.ui.showAlert('Failed to move message', 'error');
        }
    }

    async handleAttachmentClick(e) {
        e.preventDefault();
        const link = e.currentTarget;
        const { attachmentId } = link.dataset;

        try {
            this.app.ui.setLoading(link, true);
            const response = await fetch(link.href);
            const blob = await response.blob();
            
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = link.download;
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            URL.revokeObjectURL(url);
        } catch (error) {
            this.app.ui.showAlert('Failed to download attachment', 'error');
        } finally {
            this.app.ui.setLoading(link, false);
        }
    }
}

// Initialize app
document.addEventListener('DOMContentLoaded', () => {
    window.webmail = new WebmailApp();
});