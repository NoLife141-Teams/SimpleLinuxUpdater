const windowSelect = document.getElementById('window-select');
const refreshBtn = document.getElementById('refresh-btn');
const errorBanner = document.getElementById('error-banner');
const rangeLabel = document.getElementById('range-label');
const healthTrendServerSelect = document.getElementById('health-trend-server');
const observabilityInteraction = window.ObservabilityPageInteraction.createStore({ window: windowSelect.value || '7d' });
const sourceControllers = { summary: null, trends: null };
let refreshTimeoutId = null;

        function showError(message) {
            errorBanner.style.display = 'block';
            errorBanner.textContent = message;
        }

        function clearError() {
            errorBanner.style.display = 'none';
            errorBanner.textContent = '';
        }

        function formatDuration(avgMs) {
            if (!Number.isFinite(avgMs) || avgMs <= 0) return '0 ms';
            if (avgMs >= 1000) {
                return `${(avgMs / 1000).toFixed(2)} s`;
            }
            return `${avgMs.toFixed(0)} ms`;
        }

        function formatDiskKB(value) {
            const kb = Number(value || 0);
            if (!Number.isFinite(kb) || kb <= 0) return '-';
            const gb = kb / (1024 * 1024);
            if (gb >= 1) return `${gb.toFixed(1)} GB`;
            const mb = kb / 1024;
            if (mb >= 1) return `${mb.toFixed(0)} MB`;
            return `${kb.toFixed(0)} KB`;
        }

        function formatSignedNumber(value) {
            const number = Number(value || 0);
            if (!Number.isFinite(number) || number === 0) return '0';
            return number > 0 ? `+${number}` : String(number);
        }

        function appendCell(tr, text, className = '') {
            const td = document.createElement('td');
            td.textContent = text;
            if (className) {
                td.className = className;
            }
            tr.appendChild(td);
        }

        function renderTableRows(body, rows, emptyText, rowMapper) {
            body.innerHTML = '';
            if (!Array.isArray(rows) || rows.length === 0) {
                const tr = document.createElement('tr');
                const td = document.createElement('td');
                td.colSpan = 2;
                td.className = 'subtle';
                td.textContent = emptyText;
                tr.appendChild(td);
                body.appendChild(tr);
                return;
            }
            rows.forEach(row => {
                const tr = document.createElement('tr');
                rowMapper(tr, row);
                body.appendChild(tr);
            });
        }

        function describeFailureCause(cause) {
            const raw = String(cause || 'unknown').trim();
            if (!raw || raw === 'unknown') return 'Unknown failure cause';
            if (raw === 'retry_exhausted') return 'Retries exhausted before recovery';
            if (raw === 'error_class:permanent') return 'Permanent error (not retryable)';
            if (raw === 'error_class:transient') return 'Transient error (temporary issue)';
            if (raw.startsWith('error_class:')) {
                return `Error class: ${raw.slice('error_class:'.length)}`;
            }
            if (raw.startsWith('precheck:')) {
                return `Pre-check failed: ${raw.slice('precheck:'.length)}`;
            }
            if (raw.startsWith('postcheck:')) {
                return `Post-check failed: ${raw.slice('postcheck:'.length)}`;
            }
            return raw;
        }

        function sourceError(error) {
            if (error?.name === 'AbortError') return { kind: 'aborted' };
            if (error?.kind) return error;
            return { kind: 'transport', message: error?.message || String(error || 'Unknown error') };
        }

        async function loadSource(effect) {
            const controller = new AbortController();
            sourceControllers[effect.source] = { requestID: effect.requestID, controller };
            try {
                if (window.ensureAppTimezoneLoaded) {
                    await window.ensureAppTimezoneLoaded();
                }
                let url = `/api/observability/summary?window=${encodeURIComponent(effect.window)}`;
                if (effect.source === 'trends') {
                    const params = new URLSearchParams({ window: effect.queryWindow });
                    if (effect.host) params.set('server', effect.host);
                    url = `/api/observability/health-trends?${params.toString()}`;
                }
                const response = await fetch(url, { signal: controller.signal });
                if (!response.ok) {
                    throw { kind: 'http', status: response.status };
                }
                const data = await response.json();
                executeEffects(observabilityInteraction.dispatch({ type: 'sourceSucceeded', source: effect.source, requestID: effect.requestID, data, unfiltered: effect.unfiltered }));
            } catch (err) {
                executeEffects(observabilityInteraction.dispatch({ type: 'sourceFailed', source: effect.source, requestID: effect.requestID, error: sourceError(err) }));
            } finally {
                if (sourceControllers[effect.source]?.requestID === effect.requestID) sourceControllers[effect.source] = null;
            }
        }

        function errorMessage(source, state) {
            if (!state.error) return '';
            const detail = state.error.kind === 'http' ? `HTTP ${state.error.status}` : (state.error.message || state.error.kind);
            return `${source === 'summary' ? 'Summary' : 'Health trends'} ${state.status === 'stale' ? 'is stale' : 'is unavailable'} (${detail})`;
        }

        function renderAcceptedView() {
            const view = observabilityInteraction.getView();
            windowSelect.value = view.selectedWindow;
            renderHealthTrendServerOptions(view.knownHosts, view.selectedHost);
            if (view.summary.data) renderSummary(view.summary.data);
            if (view.trends.data) renderHealthTrends(view.trends.data);
            const errors = [errorMessage('summary', view.summary), errorMessage('trends', view.trends)].filter(Boolean);
            if (errors.length) showError(errors.join('; ')); else clearError();
            refreshBtn.setAttribute('aria-busy', view.refreshing ? 'true' : 'false');
        }

        function executeEffects(effects) {
            (effects || []).forEach(effect => {
                if (effect.type === 'render') renderAcceptedView();
                if (effect.type === 'loadSource') void loadSource(effect);
                if (effect.type === 'abortSource' && sourceControllers[effect.source]?.requestID === effect.requestID) sourceControllers[effect.source].controller.abort();
                if (effect.type === 'cancelRefresh' && refreshTimeoutId !== null) {
                    clearTimeout(refreshTimeoutId);
                    refreshTimeoutId = null;
                }
                if (effect.type === 'scheduleRefresh') {
                    if (refreshTimeoutId !== null) clearTimeout(refreshTimeoutId);
                    refreshTimeoutId = setTimeout(() => {
                        refreshTimeoutId = null;
                        executeEffects(observabilityInteraction.dispatch({ type: 'timerFired' }));
                    }, effect.delayMs);
                }
            });
            renderAcceptedView();
        }

        function renderSummary(summary) {
            const totals = summary?.totals || {};
            const duration = summary?.duration || {};
            const successRate = Number(totals.success_rate_pct || 0);
            const totalRuns = Number(totals.updates_total || 0);
            const avgMs = Number(duration.avg_ms || 0);
            const withDuration = Number(duration.samples_with_duration || 0);
            const withoutDuration = Number(duration.samples_without_duration || 0);
            const from = window.formatAppTimestamp
                ? window.formatAppTimestamp(summary?.from, { titleUTC: true, preformattedPrimary: summary?.from_display })
                : { primary: summary?.from || '-', title: summary?.from || '' };
            const to = window.formatAppTimestamp
                ? window.formatAppTimestamp(summary?.to, { titleUTC: true, preformattedPrimary: summary?.to_display })
                : { primary: summary?.to || '-', title: summary?.to || '' };

            document.getElementById('kpi-success-rate').textContent = `${successRate.toFixed(2)}%`;
            document.getElementById('kpi-total').textContent = String(totalRuns);
            document.getElementById('kpi-duration').textContent = formatDuration(avgMs);
            document.getElementById('kpi-duration-samples').textContent =
                `Duration samples: ${withDuration} with data, ${withoutDuration} without data`;
            rangeLabel.textContent = `Range: ${from.primary} to ${to.primary}`;
            rangeLabel.title = `UTC range: ${summary?.from || '-'} to ${summary?.to || '-'}`;

            renderTableRows(
                document.getElementById('failure-causes-body'),
                summary?.failure_causes,
                'No failure data in selected window.',
                (tr, row) => {
                    const causeCell = document.createElement('td');
                    const rawCause = String(row?.cause || 'unknown');
                    causeCell.textContent = describeFailureCause(rawCause);
                    causeCell.title = `Raw cause: ${rawCause}`;
                    tr.appendChild(causeCell);
                    appendCell(tr, String(row?.count || 0), 'bad');
                }
            );
            renderTableRows(
                document.getElementById('status-breakdown-body'),
                summary?.status_breakdown,
                'No status data in selected window.',
                (tr, row) => {
                    const statusRaw = row?.status || 'unknown';
                    const status = String(statusRaw).toLowerCase();
                    const css = status === 'success' ? 'ok' : (status === 'failure' ? 'bad' : '');
                    appendCell(tr, statusRaw);
                    appendCell(tr, String(row?.count || 0), css);
                }
            );
        }

        function renderHealthTrendServerOptions(names, selected) {
            if (!healthTrendServerSelect || !Array.isArray(names)) return;
            healthTrendServerSelect.innerHTML = '';
            const allOption = document.createElement('option');
            allOption.value = '';
            allOption.textContent = 'All hosts';
            healthTrendServerSelect.appendChild(allOption);
            names.forEach(name => {
                const option = document.createElement('option');
                option.value = name;
                option.textContent = name;
                healthTrendServerSelect.appendChild(option);
            });
            healthTrendServerSelect.value = names.includes(selected) ? selected : '';
        }

        function statusText(value) {
            const raw = String(value || '').trim();
            return raw || 'unknown';
        }

        function healthStatusClass(value) {
            const raw = String(value || '').trim().toLowerCase();
            if (raw === 'ok') return 'ok';
            if (!raw || raw === 'unknown') return '';
            return 'bad';
        }

        function appendTrendCell(tr, text, className = '', title = '') {
            const td = document.createElement('td');
            td.textContent = text;
            if (className) td.className = className;
            if (title) td.title = title;
            tr.appendChild(td);
        }

        function renderHealthTrends(trends) {
            const servers = Array.isArray(trends?.servers) ? trends.servers : [];
            const fleet = trends?.fleet || {};
            const from = window.formatAppTimestamp
                ? window.formatAppTimestamp(trends?.from, { titleUTC: true, preformattedPrimary: trends?.from_display })
                : { primary: trends?.from || '-', title: trends?.from || '' };
            const to = window.formatAppTimestamp
                ? window.formatAppTimestamp(trends?.to, { titleUTC: true, preformattedPrimary: trends?.to_display })
                : { primary: trends?.to || '-', title: trends?.to || '' };
            document.getElementById('trend-hosts').textContent = String(fleet.servers_with_samples || 0);
            document.getElementById('trend-samples').textContent = `${fleet.samples || 0} samples`;
            document.getElementById('trend-health-problems').textContent = String((fleet.apt_problem_samples || 0) + (fleet.disk_problem_samples || 0));
            document.getElementById('trend-failures').textContent = String((fleet.update_failures || 0) + (fleet.scan_failures || 0));
            const trendRangeLabel = document.getElementById('trend-range-label');
            trendRangeLabel.textContent = `Range: ${from.primary} to ${to.primary}; retention ${trends?.retention_days || 90}d`;
            trendRangeLabel.title = `UTC range: ${trends?.from || '-'} to ${trends?.to || '-'}`;

            const body = document.getElementById('health-trends-body');
            body.innerHTML = '';
            if (servers.length === 0) {
                const tr = document.createElement('tr');
                const td = document.createElement('td');
                td.colSpan = 8;
                td.className = 'subtle';
                td.textContent = 'No host health trend data in selected window.';
                tr.appendChild(td);
                body.appendChild(tr);
                return;
            }
            servers.forEach(server => {
                const latest = server.latest || {};
                const tr = document.createElement('tr');
                appendTrendCell(tr, server.name || '-', '', `${server.samples || 0} samples`);
                appendTrendCell(tr, latest.captured_at_display || latest.captured_at || '-', '', latest.captured_at || '');
                appendTrendCell(tr, `${latest.package_count || 0} (${formatSignedNumber(server.package_delta)})`);
                appendTrendCell(tr, `${latest.security_count || 0} (${formatSignedNumber(server.security_delta)})`);
                appendTrendCell(tr, `${formatDiskKB(latest.disk_free_kb)} (${formatSignedNumber(server.disk_free_delta_kb)} KB)`);
                appendTrendCell(tr, statusText(latest.apt_status), healthStatusClass(latest.apt_status));
                appendTrendCell(tr, statusText(latest.disk_status), healthStatusClass(latest.disk_status));
                const signals = [];
                if (server.update_failures) signals.push(`${server.update_failures} update fail`);
                if (server.scan_failures) signals.push(`${server.scan_failures} scan fail`);
                if (server.reboot_seen) signals.push('reboot');
                appendTrendCell(tr, signals.length ? signals.join(', ') : 'none');
                body.appendChild(tr);
            });
        }

        refreshBtn.addEventListener('click', () => executeEffects(observabilityInteraction.dispatch({ type: 'manualRefresh' })));
        windowSelect.addEventListener('change', () => executeEffects(observabilityInteraction.dispatch({ type: 'windowChanged', window: windowSelect.value })));
        healthTrendServerSelect?.addEventListener('change', () => executeEffects(observabilityInteraction.dispatch({ type: 'hostChanged', host: healthTrendServerSelect.value })));
        document.getElementById('logout-btn').addEventListener('click', () => window.logout());
        document.addEventListener('visibilitychange', () => {
            if (document.hidden) {
                executeEffects(observabilityInteraction.dispatch({ type: 'pageHidden' }));
                return;
            }
            executeEffects(observabilityInteraction.dispatch({ type: 'pageShown' }));
        });

        if (!document.hidden) {
            executeEffects(observabilityInteraction.dispatch({ type: 'pageShown' }));
        }
