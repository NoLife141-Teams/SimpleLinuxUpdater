// A mounted browser adapter owns effects, never accepted domain state.
(function (root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.BrowserEffects = api;
})(globalThis, function () {
    "use strict";
    function create(environment = globalThis) {
        let active = false;
        let generation = 0;
        const cleanups = new Set();
        const timers = new Map();
        const requests = new Map();
        let listeners = new WeakMap();
        const current = (ticket) => active && ticket === generation;
        const stale = () =>
            new DOMException("Adapter was destroyed", "AbortError");
        function init() {
            if (!active) {
                active = true;
                generation++;
            }
        }
        // Return an unregister function for effects completed before destruction.
        function own(cleanup) {
            cleanups.add(cleanup);
            return () => cleanups.delete(cleanup);
        }
        function listen(target, type, callback, options) {
            if (!active || !target) return;
            const previous = listeners.get(target)?.get(callback)?.get(type);
            if (previous) return previous.cleanup;
            const ticket = generation;
            const listener = (event) => {
                if (current(ticket)) callback(event);
            };
            target.addEventListener(type, listener, options);
            // Replaced table rows must remain collectible between renders.
            const weakTarget = new WeakRef(target),
                weakListener = new WeakRef(listener);
            const cleanup = () =>
                weakTarget
                    .deref()
                    ?.removeEventListener(type, weakListener.deref(), options);
            const unregister = own(cleanup);
            let entries = listeners.get(target);
            if (!entries) {
                entries = new Map();
                listeners.set(target, entries);
            }
            let types = entries.get(callback);
            if (!types) {
                types = new Map();
                entries.set(callback, types);
            }
            types.set(type, { cleanup, unregister });
            return cleanup;
        }
        function unlisten(target, type, callback) {
            const entries = listeners.get(target),
                types = entries?.get(callback);
            const entry = types?.get(type);
            if (!entry) return;
            entry.cleanup();
            entry.unregister();
            types.delete(type);
            if (!types.size) entries.delete(callback);
        }
        function schedule(kind, callback, delay) {
            if (!active) return null;
            const ticket = generation;
            const id = environment[kind]((...args) => {
                if (kind !== "setInterval") timers.delete(`${kind}:${id}`);
                if (current(ticket)) callback(...args);
            }, delay);
            timers.set(`${kind}:${id}`, { kind, id });
            return id;
        }
        function clear(id, kind) {
            if (!timers.has(`${kind}:${id}`)) return;
            environment[
                kind === "setInterval"
                    ? "clearInterval"
                    : kind === "requestAnimationFrame"
                      ? "cancelAnimationFrame"
                      : "clearTimeout"
            ](id);
            timers.delete(`${kind}:${id}`);
        }
        async function fetchOwned(url, options = {}) {
            if (!active) throw stale();
            const ticket = generation,
                controller = new AbortController();
            const abortExternal = () => controller.abort();
            const release = () => {
                options.signal?.removeEventListener("abort", abortExternal);
                requests.delete(controller);
            };
            requests.set(controller, release);
            if (options.signal) {
                if (options.signal.aborted) controller.abort();
                else
                    options.signal.addEventListener("abort", abortExternal, {
                        once: true,
                    });
            }
            try {
                const response = await environment.fetch(url, {
                    ...options,
                    signal: controller.signal,
                });
                if (!current(ticket)) throw stale();
                const wrap = (response) =>
                    new Proxy(response, {
                        get(target, key) {
                            const value = Reflect.get(target, key, target);
                            if (key === "clone")
                                return () => wrap(target.clone());
                            if (
                                [
                                    "json",
                                    "text",
                                    "blob",
                                    "arrayBuffer",
                                    "formData",
                                ].includes(key)
                            )
                                return async () => {
                                    try {
                                        const data = await value.call(target);
                                        if (!current(ticket)) throw stale();
                                        return data;
                                    } finally {
                                        release();
                                    }
                                };
                            return typeof value === "function"
                                ? value.bind(target)
                                : value;
                        },
                    });
                // Retain the controller through body consumption or destruction.
                if (response.body === null) release();
                return wrap(response);
            } catch (error) {
                release();
                throw error;
            }
        }
        function observer(Type) {
            return class {
                constructor(callback, options) {
                    const ticket = generation;
                    this.native = new Type((...args) => {
                        if (current(ticket)) callback(...args);
                    }, options);
                    own(() => this.native.disconnect());
                }
                observe(...args) {
                    this.native.observe(...args);
                }
                unobserve(...args) {
                    this.native.unobserve(...args);
                }
                disconnect() {
                    this.native.disconnect();
                }
            };
        }
        function destroy() {
            if (!active) return;
            active = false;
            generation++;
            for (const { id, kind } of timers.values()) clear(id, kind);
            for (const [request, release] of requests) {
                request.abort();
                release();
            }
            requests.clear();
            for (const cleanup of Array.from(cleanups).reverse()) cleanup();
            cleanups.clear();
            listeners = new WeakMap();
        }
        return Object.freeze({
            get active() {
                return active;
            },
            capture: () => generation,
            isCurrent: current,
            init,
            destroy,
            own,
            listen,
            unlisten,
            fetch: fetchOwned,
            observer,
            setTimeout: (callback, delay) =>
                schedule("setTimeout", callback, delay),
            clearTimeout: (id) => clear(id, "setTimeout"),
            setInterval: (callback, delay) =>
                schedule("setInterval", callback, delay),
            clearInterval: (id) => clear(id, "setInterval"),
            requestAnimationFrame: (callback) =>
                schedule("requestAnimationFrame", callback),
            cancelAnimationFrame: (id) => clear(id, "requestAnimationFrame"),
        });
    }
    return Object.freeze({ create });
});
