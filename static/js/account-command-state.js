// Account command admission contains no credentials or returned secrets.
(function (root, factory) {
    const api = factory();
    if (typeof module === 'object' && module.exports) module.exports = api;
    if (root) root.AccountCommandState = api;
})(globalThis, function () {
    function create() {
        let active = true, generation = 0, pending = null, secret = '', frozen = false;
        const allowed = operation => active && !pending && !frozen && (!secret || (secret === 'enrollment' && operation === 'mfa-confirm'));
        return Object.freeze({
            allowed,
            begin(operation) { if (!allowed(operation)) return null; return pending = Object.freeze({ operation, generation: ++generation }); },
            current(ticket) { return active && pending === ticket && ticket.generation === generation; },
            finish(ticket) { if (pending === ticket) pending = null; },
            protect(kind) { secret = kind; if (kind === 'recovery') frozen = true; },
            acknowledge() { secret = ''; generation++; },
            destroy() { active = false; generation++; pending = null; secret = ''; },
            view: () => ({ active, busy: pending?.operation || '', secret, frozen }),
        });
    }
    return Object.freeze({ create });
});
