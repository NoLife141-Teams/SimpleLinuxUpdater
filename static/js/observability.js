const windowSelect = document.getElementById('window-select');
const refreshBtn = document.getElementById('refresh-btn');
const errorBanner = document.getElementById('error-banner');
const rangeLabel = document.getElementById('range-label');
const healthTrendServerSelect = document.getElementById('health-trend-server');
const healthSearch = document.getElementById('health-search');
const healthAttentionFilter = document.getElementById('health-attention-filter');
const healthSort = document.getElementById('health-sort');
const initialQuery = new URLSearchParams(window.location.search);
const observabilityInteraction = window.ObservabilityPageInteraction.createStore({
    window: initialQuery.get('window') || windowSelect.value || '7d',
    host: initialQuery.get('host') || '',
    search: initialQuery.get('search') || '',
    attention: initialQuery.get('attention') || 'all',
    sort: initialQuery.get('sort') || 'attention',
    page: initialQuery.get('page') || 1,
});
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

        function plural(count, singular, pluralForm = `${singular}s`) {
            return `${count} ${count === 1 ? singular : pluralForm}`;
        }

        function formatRelativeTime(raw) {
            const timestamp = Date.parse(raw || '');
            if (!Number.isFinite(timestamp)) return '';
            const seconds = Math.max(0, Math.floor((Date.now() - timestamp) / 1000));
            if (seconds < 60) return `${seconds}s ago`;
            const minutes = Math.floor(seconds / 60);
            if (minutes < 60) return `${minutes}m ago`;
            const hours = Math.floor(minutes / 60);
            if (hours < 48) return `${hours}h ago`;
            return `${Math.floor(hours / 24)}d ago`;
        }

        function renderSourceLifecycle(source, projection) {
            const container = document.getElementById(`${source}-lifecycle`);
            const section = document.getElementById(source === 'summary' ? 'observability-summary' : 'observability-trends');
            if (!container || !section) return;
            container.dataset.state = projection.state;
            container.querySelector('.source-lifecycle-status').textContent = projection.label;
            const relative = formatRelativeTime(projection.acceptedAt);
            let detail = projection.detail;
            if (projection.acceptedAt && projection.state === 'current') detail = `Updated ${relative}`;
            if (projection.acceptedAt && ['refreshing', 'stale'].includes(projection.state)) detail = `Last accepted ${relative}`;
            container.querySelector('.source-lifecycle-time').textContent = detail;
            container.querySelector('.source-retry').hidden = !projection.retry;
            section.setAttribute('aria-busy', String(projection.state === 'loading' || projection.state === 'refreshing'));
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

        function formatDelta(value, noun) {
            const number = Number(value || 0);
            if (!Number.isFinite(number) || number === 0) return `unchanged ${noun}`;
            return `${Math.abs(number)} ${noun} ${number > 0 ? 'increase' : 'decrease'}`;
        }

        function validDiskKB(value) {
            const disk = Number(value);
            return Number.isFinite(disk) && disk > 0 ? disk : null;
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

        function renderBreakdownBars(containerID, rows, label, describe) {
            const container = document.getElementById(containerID);
            container.innerHTML = '';
            const acceptedRows = Array.isArray(rows) ? rows : [];
            const total = acceptedRows.reduce((sum, row) => sum + Number(row?.count || 0), 0);
            acceptedRows.forEach(row => {
                const count = Number(row?.count || 0);
                const item = document.createElement('div');
                item.className = 'breakdown-bar';
                const heading = document.createElement('div');
                heading.className = 'breakdown-bar-heading';
                const name = document.createElement('span');
                name.textContent = describe(row);
                const value = document.createElement('strong');
                value.textContent = `${count} · ${total > 0 ? ((count / total) * 100).toFixed(1) : '0.0'}%`;
                heading.append(name, value);
                const progress = document.createElement('progress');
                progress.max = Math.max(1, total);
                progress.value = count;
                progress.setAttribute('aria-label', `${label}: ${describe(row)}, ${count} of ${total}`);
                item.append(heading, progress);
                container.appendChild(item);
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
            healthSearch.value = view.search;
            healthAttentionFilter.value = view.attention;
            healthSort.value = view.sort;
            renderHealthTrendServerOptions(view.knownHosts, view.selectedHost);
            if (view.summary.data) renderSummary(view.summary.data);
            if (view.trends.data) renderHealthTrends(view.trends.data, view);
            renderSourceLifecycle('summary', view.summaryLifecycle);
            renderSourceLifecycle('trends', view.trendsLifecycle);
            const errors = [errorMessage('summary', view.summary), errorMessage('trends', view.trends)].filter(Boolean);
            if (errors.length) showError(errors.join('; ')); else clearError();
            refreshBtn.setAttribute('aria-busy', view.refreshing ? 'true' : 'false');
            refreshBtn.disabled = view.refreshing;
            const liveStatus = document.getElementById('observability-live-status');
            const announcement = view.refreshing
                ? 'Refreshing observability data.'
                : `Observability refresh complete. Summary ${view.summaryLifecycle.label}; health trends ${view.trendsLifecycle.label}.`;
            if (liveStatus.textContent !== announcement) liveStatus.textContent = announcement;
            const acceptedTimes = [
                view.summary.data?.generated_at || view.summary.data?.to,
                view.trends.data?.generated_at || view.trends.data?.to,
            ].map(Date.parse).filter(Number.isFinite);
            if (acceptedTimes.length) {
                document.getElementById('observability-last-refresh').textContent =
                    `Latest accepted · ${formatRelativeTime(new Date(Math.max(...acceptedTimes)).toISOString())}`;
            }
            updateShareableURL(view);
        }

        function updateShareableURL(view) {
            const params = new URLSearchParams();
            params.set('window', view.selectedWindow);
            if (view.selectedHost) params.set('host', view.selectedHost);
            if (view.search) params.set('search', view.search);
            if (view.attention !== 'all') params.set('attention', view.attention);
            if (view.sort !== 'attention') params.set('sort', view.sort);
            if (view.page > 1) params.set('page', String(view.page));
            const next = `${window.location.pathname}?${params.toString()}`;
            if (`${window.location.pathname}${window.location.search}` !== next) {
                window.history.replaceState(null, '', next);
            }
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
            const successRuns = Number(totals.updates_success || 0);
            const failedRuns = Number(totals.updates_failure || 0);
            const avgMs = Number(duration.avg_ms || 0);
            const withDuration = Number(duration.samples_with_duration || 0);
            const withoutDuration = Number(duration.samples_without_duration || 0);
            const from = window.formatAppTimestamp
                ? window.formatAppTimestamp(summary?.from, { titleUTC: true, preformattedPrimary: summary?.from_display })
                : { primary: summary?.from || '-', title: summary?.from || '' };
            const to = window.formatAppTimestamp
                ? window.formatAppTimestamp(summary?.to, { titleUTC: true, preformattedPrimary: summary?.to_display })
                : { primary: summary?.to || '-', title: summary?.to || '' };

            const severity = window.ObservabilityPageInteraction.projectFleetHealth({
                updatesTotal: totalRuns,
                successRate,
            });
            const successCard = document.getElementById('kpi-success-rate-card');
            successCard.dataset.severity = severity.state;
            document.getElementById('kpi-success-rate').textContent = totalRuns > 0 ? `${successRate.toFixed(2)}% · ${severity.label}` : 'No data';
            document.getElementById('kpi-success-context').textContent =
                totalRuns > 0 ? `${plural(successRuns, 'successful run')} · ${plural(failedRuns, 'failed run')}` : 'No update runs in selected window';
            document.getElementById('kpi-total').textContent = String(totalRuns);
            document.getElementById('kpi-duration').textContent = formatDuration(avgMs);
            const durationCard = document.getElementById('kpi-duration-card');
            const confidence = window.ObservabilityPageInteraction.projectDurationConfidence({
                samples: withDuration,
                total: totalRuns,
            });
            durationCard.dataset.confidence = confidence.state;
            document.getElementById('kpi-duration-samples').textContent =
                `${confidence.label} · ${withDuration} of ${totalRuns} runs with duration · ${withoutDuration} missing`;
            rangeLabel.textContent = `Range: ${from.primary} to ${to.primary}`;
            rangeLabel.title = `UTC range: ${summary?.from || '-'} to ${summary?.to || '-'}`;

            renderTableRows(
                document.getElementById('failure-causes-body'),
                summary?.failure_causes,
                'No failure data in selected window.',
                (tr, row) => {
                    const causeCell = document.createElement('td');
                    const rawCause = String(row?.cause || 'unknown');
                    const link = document.createElement('a');
                    link.href = '/manage?audit_action=update.complete&audit_status=failure';
                    link.textContent = describeFailureCause(rawCause);
                    causeCell.appendChild(link);
                    if (!rawCause || rawCause === 'unknown') {
                        const quality = document.createElement('span');
                        quality.className = 'status-pill pill-warning inline-quality-pill';
                        quality.textContent = 'Data quality issue';
                        causeCell.appendChild(quality);
                    }
                    const servers = Array.isArray(row?.servers) ? row.servers : [];
                    if (servers.length) {
                        const serverLinks = document.createElement('div');
                        serverLinks.className = 'table-shell-subtle';
                        serverLinks.append('Affected: ');
                        servers.forEach((server, index) => {
                            if (index) serverLinks.append(', ');
                            const serverLink = document.createElement('a');
                            serverLink.href = `/manage?audit_target=${encodeURIComponent(server)}&audit_action=update.complete&audit_status=failure`;
                            serverLink.textContent = server;
                            serverLinks.appendChild(serverLink);
                        });
                        causeCell.appendChild(serverLinks);
                    }
                    causeCell.title = `Raw cause: ${rawCause}`;
                    tr.appendChild(causeCell);
                    appendCell(tr, String(row?.count || 0), 'bad');
                }
            );
            renderBreakdownBars(
                'failure-breakdown-bars',
                summary?.failure_causes,
                'Failure causes',
                row => describeFailureCause(row?.cause)
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
            renderBreakdownBars(
                'status-breakdown-bars',
                summary?.status_breakdown,
                'Run statuses',
                row => String(row?.status || 'unknown')
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
            if (selected && !names.includes(selected)) {
                const selectedOption = document.createElement('option');
                selectedOption.value = selected;
                selectedOption.textContent = selected;
                healthTrendServerSelect.appendChild(selectedOption);
            }
            healthTrendServerSelect.value = selected || '';
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

        function appendBadgeCell(tr, text, state) {
            const td = document.createElement('td');
            const badge = document.createElement('span');
            badge.className = `status-pill health-signal-badge ${state || 'status-unknown'}`;
            badge.textContent = text;
            td.appendChild(badge);
            tr.appendChild(td);
        }

        function renderTrendChart(containerID, series, label, formatter = value => String(value)) {
            const container = document.getElementById(containerID);
            container.innerHTML = '';
            const valid = series.filter(point => Number.isFinite(point.value));
            if (valid.length === 0) {
                const empty = document.createElement('p');
                empty.className = 'muted trend-chart-empty';
                empty.textContent = 'No chart samples in this window.';
                container.appendChild(empty);
                return;
            }
            const namespace = 'http://www.w3.org/2000/svg';
            const svg = document.createElementNS(namespace, 'svg');
            svg.setAttribute('viewBox', '0 0 320 100');
            svg.setAttribute('role', 'img');
            svg.setAttribute('aria-label', `${label} trend, ${valid.length} time points`);
            const minimum = Math.min(...valid.map(point => point.value));
            const maximum = Math.max(...valid.map(point => point.value));
            const span = maximum - minimum || 1;
            const positions = valid.map((point, index) => ({
                ...point,
                x: valid.length === 1 ? 160 : 10 + (index / (valid.length - 1)) * 300,
                y: 90 - ((point.value - minimum) / span) * 80,
            }));
            const line = document.createElementNS(namespace, 'polyline');
            line.setAttribute('points', positions.map(point => `${point.x},${point.y}`).join(' '));
            line.setAttribute('class', 'trend-line');
            svg.appendChild(line);
            positions.forEach(point => {
                const circle = document.createElementNS(namespace, 'circle');
                circle.setAttribute('cx', String(point.x));
                circle.setAttribute('cy', String(point.y));
                circle.setAttribute('r', '3.5');
                circle.setAttribute('tabindex', '0');
                circle.setAttribute('role', 'button');
                circle.setAttribute('aria-label', `${label}: ${formatter(point.value)} at ${point.timestamp}`);
                circle.setAttribute('class', 'trend-point');
                svg.appendChild(circle);
            });
            container.appendChild(svg);
        }

        function renderTrendCharts(series) {
            const { packages, security, disk, failures } = series;
            renderTrendChart('package-trend-chart', packages, 'Package count');
            renderTrendChart('security-trend-chart', security, 'Security update count');
            renderTrendChart('disk-trend-chart', disk, 'Disk free', formatDiskKB);
            renderTrendChart('failure-trend-chart', failures, 'Failure event count');
            const summaries = [
                ['package-chart-summary', packages, value => plural(value, 'package')],
                ['security-chart-summary', security, value => plural(value, 'security update')],
                ['disk-chart-summary', disk, formatDiskKB],
                ['failure-chart-summary', failures, value => plural(value, 'failure event')],
            ];
            summaries.forEach(([id, series, formatter]) => {
                const latest = series[series.length - 1];
                document.getElementById(id).textContent = latest ? `Latest ${formatter(latest.value)}` : 'No samples';
            });
        }

        function renderHealthTrends(trends, view) {
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
            document.getElementById('observability-retention').textContent = `Retention · ${trends?.retention_days || 90}d`;
            renderTrendCharts(view.trendSeries);

            const body = document.getElementById('health-trends-body');
            body.innerHTML = '';
            const { items, visibleItems, page, pageCount, staleNames } = view.health;
            document.getElementById('health-result-count').textContent = `${visibleItems.length} of ${items.length} hosts`;
            document.getElementById('health-page-label').textContent = `Page ${page} of ${pageCount}`;
            document.getElementById('health-previous-page').disabled = page <= 1;
            document.getElementById('health-next-page').disabled = page >= pageCount;
            if (visibleItems.length === 0) {
                const tr = document.createElement('tr');
                const td = document.createElement('td');
                td.colSpan = 8;
                td.className = 'subtle';
                td.textContent = servers.length ? 'No hosts match the current filters.' : 'No host health trend data in selected window.';
                tr.appendChild(td);
                body.appendChild(tr);
                return;
            }
            visibleItems.forEach(server => {
                const latest = server.latest || {};
                const tr = document.createElement('tr');
                const hostCell = document.createElement('td');
                const hostLink = document.createElement('a');
                hostLink.href = `/manage?audit_target=${encodeURIComponent(server.name || '')}`;
                hostLink.textContent = server.name || '-';
                hostLink.title = `${server.samples || 0} health samples; open audit investigation`;
                hostCell.appendChild(hostLink);
                tr.appendChild(hostCell);
                const captured = latest.captured_at_display || latest.captured_at || '-';
                const latestCell = document.createElement('td');
                latestCell.textContent = `${captured}${latest.captured_at ? ` · ${formatRelativeTime(latest.captured_at)}` : ''}`;
                latestCell.title = latest.captured_at || '';
                if (staleNames.includes(server.name)) {
                    latestCell.className = 'warning';
                    const stale = document.createElement('span');
                    stale.className = 'status-pill pill-warning inline-quality-pill';
                    stale.textContent = 'Stale';
                    latestCell.append(' ', stale);
                }
                tr.appendChild(latestCell);
                appendTrendCell(tr, `${latest.package_count || 0} · ${formatDelta(server.package_delta, 'package')}`);
                appendTrendCell(tr, `${latest.security_count || 0} · ${formatDelta(server.security_delta, 'security update')}`);
                const latestDisk = Number(latest.disk_free_kb || 0);
                const firstDisk = Number(server.first?.disk_free_kb || 0);
                const diskText = latestDisk > 0
                    ? `${formatDiskKB(latestDisk)}${firstDisk > 0 ? ` (${formatDiskKB(Math.abs(server.disk_free_delta_kb || 0))} ${Number(server.disk_free_delta_kb || 0) < 0 ? 'decrease' : 'increase'})` : ''}`
                    : 'Unavailable';
                appendTrendCell(tr, diskText, latestDisk > 0 ? '' : 'muted');
                appendBadgeCell(tr, statusText(latest.apt_status), healthStatusClass(latest.apt_status) === 'ok' ? 'status-success' : 'status-error');
                appendBadgeCell(tr, statusText(latest.disk_status), healthStatusClass(latest.disk_status) === 'ok' ? 'status-success' : 'status-error');
                const signals = [];
                if (server.update_failures) signals.push(plural(server.update_failures, 'update failure'));
                if (server.scan_failures) signals.push(plural(server.scan_failures, 'scan failure'));
                if (server.reboot_seen) signals.push('Reboot required');
                appendBadgeCell(tr, signals.length ? signals.join(' · ') : 'No signals', signals.length ? 'status-error' : 'status-success');
                body.appendChild(tr);
            });
        }

        function csvValue(value) {
            const text = String(value ?? '');
            return /[",\n]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text;
        }

        function downloadHealthCSV() {
            const header = ['Host', 'Captured at app time', 'Captured at UTC', 'Packages', 'Security updates', 'Disk free KB', 'APT status', 'Disk status', 'Update failures', 'Scan failures', 'Reboot required'];
            const rows = observabilityInteraction.getView().health.items.map(server => {
                const latest = server.latest || {};
                return [
                    server.name,
                    latest.captured_at_display || '',
                    latest.captured_at,
                    latest.package_count,
                    latest.security_count,
                    validDiskKB(latest.disk_free_kb) || '',
                    latest.apt_status,
                    latest.disk_status,
                    server.update_failures,
                    server.scan_failures,
                    server.reboot_seen ? 'yes' : 'no',
                ];
            });
            const csv = [header, ...rows].map(row => row.map(csvValue).join(',')).join('\n');
            downloadCSV(csv, `observability-health-${observabilityInteraction.getView().selectedWindow}.csv`);
        }

        function downloadCSV(csv, filename) {
            const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
            const url = URL.createObjectURL(blob);
            const anchor = document.createElement('a');
            anchor.href = url;
            anchor.download = filename;
            document.body.appendChild(anchor);
            anchor.click();
            anchor.remove();
            URL.revokeObjectURL(url);
        }

        function downloadFailuresCSV() {
            const summary = observabilityInteraction.getView().summary.data;
            const rows = Array.isArray(summary?.failure_causes) ? summary.failure_causes : [];
            const csv = [
                ['Cause', 'Count'],
                ...rows.map(row => [describeFailureCause(row?.cause), Number(row?.count || 0)]),
            ].map(row => row.map(csvValue).join(',')).join('\n');
            downloadCSV(csv, `observability-failures-${observabilityInteraction.getView().selectedWindow}.csv`);
        }

        refreshBtn.addEventListener('click', () => executeEffects(observabilityInteraction.dispatch({ type: 'manualRefresh' })));
        document.querySelectorAll('[data-retry-source]').forEach(button => {
            button.addEventListener('click', () => executeEffects(observabilityInteraction.dispatch({ type: 'retrySource', source: button.dataset.retrySource })));
        });
        windowSelect.addEventListener('change', () => executeEffects(observabilityInteraction.dispatch({ type: 'windowChanged', window: windowSelect.value })));
        healthTrendServerSelect?.addEventListener('change', () => executeEffects(observabilityInteraction.dispatch({ type: 'hostChanged', host: healthTrendServerSelect.value })));
        healthSearch?.addEventListener('input', () => executeEffects(observabilityInteraction.dispatch({
            type: 'filtersChanged',
            search: healthSearch.value,
            attention: healthAttentionFilter.value,
            sort: healthSort.value,
        })));
        healthAttentionFilter?.addEventListener('change', () => executeEffects(observabilityInteraction.dispatch({
            type: 'filtersChanged',
            search: healthSearch.value,
            attention: healthAttentionFilter.value,
            sort: healthSort.value,
        })));
        healthSort?.addEventListener('change', () => executeEffects(observabilityInteraction.dispatch({
            type: 'filtersChanged',
            search: healthSearch.value,
            attention: healthAttentionFilter.value,
            sort: healthSort.value,
        })));
        document.getElementById('health-previous-page')?.addEventListener('click', () => {
            const view = observabilityInteraction.getView();
            executeEffects(observabilityInteraction.dispatch({ type: 'pageChanged', page: view.page - 1 }));
        });
        document.getElementById('health-next-page')?.addEventListener('click', () => {
            const view = observabilityInteraction.getView();
            executeEffects(observabilityInteraction.dispatch({ type: 'pageChanged', page: view.page + 1 }));
        });
        document.getElementById('export-health-csv')?.addEventListener('click', downloadHealthCSV);
        document.getElementById('export-failures-csv')?.addEventListener('click', downloadFailuresCSV);
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
