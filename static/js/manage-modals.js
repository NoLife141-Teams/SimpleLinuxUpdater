// Manage page modal focus and file-label helpers. Loaded before manage.js.
let modalFocusStack = [];

function modalFocusableElements(modal) {
    if (!modal) return [];
    return Array.from(modal.querySelectorAll([
        'a[href]',
        'button:not([disabled])',
        'input:not([disabled]):not([type="hidden"])',
        'select:not([disabled])',
        'textarea:not([disabled])',
        '[tabindex]:not([tabindex="-1"])'
    ].join(','))).filter((el) => {
        return !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
    });
}

function activateModalFocus(modal, preferredTarget) {
    if (!modal) return;
    modalFocusStack = modalFocusStack.filter((entry) => entry.modal !== modal);
    modalFocusStack.push({ modal, previous: document.activeElement });
    const focusable = modalFocusableElements(modal);
    const target = preferredTarget && !preferredTarget.disabled ? preferredTarget : focusable[0];
    if (target && typeof target.focus === 'function') {
        window.setTimeout(() => target.focus({ preventScroll: true }), 0);
    }
}

function releaseModalFocus(modal) {
    const index = modalFocusStack.map((entry) => entry.modal).lastIndexOf(modal);
    if (index === -1) return;
    const [entry] = modalFocusStack.splice(index, 1);
    const previous = entry.previous;
    if (previous && document.contains(previous) && typeof previous.focus === 'function') {
        window.setTimeout(() => previous.focus({ preventScroll: true }), 0);
    }
}

function trapActiveModalFocus(event) {
    const entry = modalFocusStack[modalFocusStack.length - 1];
    if (!entry || !entry.modal || !entry.modal.classList.contains('active')) {
        return false;
    }
    const focusable = modalFocusableElements(entry.modal);
    if (!focusable.length) {
        event.preventDefault();
        return true;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
        return true;
    }
    if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
        return true;
    }
    if (!entry.modal.contains(document.activeElement)) {
        event.preventDefault();
        first.focus();
        return true;
    }
    return false;
}

function fileInputEmptyLabel(input) {
    if (!input) return 'Choose file';
    switch (input.id) {
        case 'global-key-file':
            return 'Choose global key';
        case 'key_file':
            return 'Choose host key';
        case 'edit-key':
            return 'Choose key';
        default:
            return 'Choose file';
    }
}

function resetFileInputLabel(input) {
    updateFileLabel(input, fileInputEmptyLabel(input));
}
