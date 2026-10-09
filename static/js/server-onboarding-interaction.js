(function (root, factory) {
    if (typeof module === 'object' && module.exports) module.exports = factory();
    else root.ServerOnboardingInteraction = factory();
}(typeof globalThis !== 'undefined' ? globalThis : this, function () {
    'use strict';
    function createStore() {
        let revision = 0;
        let state = { step: 1, busy: false, scan: null, trusted: false, report: null, savedName: '' };
        function reset() {
            if (state.busy) return false;
            revision++;
            state = { step: 1, busy: false, scan: null, trusted: false, report: null, savedName: '' };
            return true;
        }
        function begin(kind) {
            if (state.busy) return null;
            if (kind === 'scan' && state.step !== 1) return null;
            if (kind === 'check' && !((state.step === 2 || state.step === 3) && state.trusted && state.scan?.fingerprint_sha256)) return null;
            if (kind === 'create' && !(state.step === 4 && state.report?.ready && state.trusted)) return null;
            state.busy = true;
            if (kind === 'check') { state.step = 3; state.report = null; }
            return { revision: ++revision, kind };
        }
        function finish(request, data, failed = false) {
            if (!request || request.revision !== revision || !state.busy) return false;
            state.busy = false;
            if (failed) return true;
            if (request.kind === 'scan') { state.scan = data; state.trusted = false; state.step = 2; }
            if (request.kind === 'check') state.report = data;
            if (request.kind === 'create') { state.savedName = data.name; state.step = 5; }
            return true;
        }
        function trust(confirmed) {
            if (!state.busy && state.step === 2) state.trusted = !!confirmed;
        }
        function next() {
            if (!state.busy && state.step === 3 && state.report?.ready) { state.step = 4; return true; }
            return false;
        }
        function back() {
            if (state.busy || state.step === 1 || state.step === 5) return false;
            if (state.step === 4) state.step = 3;
            else { reset(); }
            return true;
        }
        return Object.freeze({ getView: () => JSON.parse(JSON.stringify(state)), begin, finish, trust, next, back, reset });
    }
    return Object.freeze({ createStore });
}));
