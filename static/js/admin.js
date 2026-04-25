let timer = null;
let countdown = null;
let remaining = 0;

function refreshStats() {
    fetch('/admin/stats')
        .then(r => r.json())
        .then(users => {
            users.forEach(u => {
                const card = document.querySelector(`[data-user-id="${u.user_id}"]`);
                if (!card) return;

                const userOnline = card.querySelector('[data-stat="user-online"]');
                if (userOnline) {
                    if (u.online_count > 0) {
                        userOnline.className = 'badge badge-green';
                        userOnline.textContent = 'online (' + u.online_count + ')';
                    } else {
                        userOnline.className = 'badge badge-gray';
                        userOnline.textContent = 'offline';
                    }
                }

                const userTotal = card.querySelector('[data-stat="user-total"]');
                if (userTotal) userTotal.textContent = 'Σ ' + u.total_traffic_fmt;

                const userBalance = card.querySelector('[data-stat="user-balance"]');
                if (userBalance && u.balance_fmt) userBalance.textContent = u.balance_fmt;

                // На карточке /admin/users/{id} — подробные счётчики подписки.
                // В списке /admin этих элементов нет, querySelector вернёт null
                // и обновление пропустится.
                const userBase = card.querySelector('[data-stat="user-base"]');
                if (userBase && u.base_fmt) userBase.textContent = u.base_fmt;

                const userExtra = card.querySelector('[data-stat="user-extra"]');
                if (userExtra && u.extra_fmt) userExtra.textContent = u.extra_fmt;

                // Прогресс базового трафика подписки
                const baseRow = card.querySelector('[data-stat="user-base-row"]');
                if (baseRow) baseRow.style.display = u.base_limit > 0 ? '' : 'none';
                if (u.base_limit > 0) {
                    const baseUsed = card.querySelector('[data-stat="user-base-used"]');
                    const baseLimit = card.querySelector('[data-stat="user-base-limit"]');
                    const basePct = card.querySelector('[data-stat="user-base-pct"]');
                    const baseBar = card.querySelector('[data-stat="user-base-bar"]');
                    if (baseUsed) baseUsed.textContent = u.base_used_fmt;
                    if (baseLimit) baseLimit.textContent = u.base_limit_fmt;
                    if (basePct) basePct.textContent = u.base_percent;
                    if (baseBar) {
                        baseBar.style.width = u.base_percent + '%';
                        baseBar.style.background = u.base_percent >= 90 ? '#ef4444'
                            : u.base_percent >= 70 ? '#f59e0b' : '#22c55e';
                    }
                }

                // Прогресс докупленного трафика
                const extraRow = card.querySelector('[data-stat="user-extra-row"]');
                if (extraRow) extraRow.style.display = u.extra_granted > 0 ? '' : 'none';
                if (u.extra_granted > 0) {
                    const extraUsed = card.querySelector('[data-stat="user-extra-used"]');
                    const extraGranted = card.querySelector('[data-stat="user-extra-granted"]');
                    const extraPct = card.querySelector('[data-stat="user-extra-pct"]');
                    const extraBar = card.querySelector('[data-stat="user-extra-bar"]');
                    if (extraUsed) extraUsed.textContent = u.extra_used_fmt;
                    if (extraGranted) extraGranted.textContent = u.extra_granted_fmt;
                    if (extraPct) extraPct.textContent = u.extra_percent;
                    if (extraBar) {
                        extraBar.style.width = u.extra_percent + '%';
                        extraBar.style.background = u.extra_percent >= 90 ? '#ef4444'
                            : u.extra_percent >= 70 ? '#f59e0b' : '#fcd34d';
                    }
                }

                (u.profiles || []).forEach(p => {
                    const row = card.querySelector(`[data-profile-id="${p.id}"]`);
                    if (!row) return;

                    const status = row.querySelector('[data-stat="profile-status"]');
                    const ipsEl = row.querySelector('[data-stat="profile-ips"]');
                    const traffic = row.querySelector('[data-stat="profile-traffic"]');

                    if (status) {
                        if (!p.is_active) {
                            status.className = 'badge badge-red';
                            if (p.is_expired) status.textContent = 'expired';
                            else if (p.is_over_limit) status.textContent = 'лимит';
                            else status.textContent = 'disabled';
                        } else if (p.is_online) {
                            status.className = 'badge badge-green';
                            status.textContent = '● online';
                        } else {
                            status.className = 'badge badge-blue';
                            status.textContent = 'active';
                        }
                    }

                    if (ipsEl) {
                        const ips = p.online_ips || [];
                        if (p.is_active && p.is_online && ips.length > 0) {
                            ipsEl.innerHTML = ips.map(ip => '<span class="badge badge-blue">' + ip + '</span> ').join('');
                        } else {
                            ipsEl.innerHTML = '';
                        }
                    }

                    // Кнопки Откл/Вкл — переключаем по is_active.
                    const btnDeact = row.querySelector('[data-stat="profile-toggle-deactivate"]');
                    const btnAct = row.querySelector('[data-stat="profile-toggle-activate"]');
                    if (btnDeact) btnDeact.style.display = p.is_active ? '' : 'none';
                    if (btnAct) btnAct.style.display = p.is_active ? 'none' : '';

                    if (traffic) traffic.textContent = '↑' + p.traffic_up_fmt + ' ↓' + p.traffic_down_fmt;

                    // Per-profile прогресс-бар (только на /admin/users/{id}).
                    const usageText = row.querySelector('[data-stat="profile-usage-text"]');
                    const barWrap = row.querySelector('[data-stat="profile-bar-wrap"]');
                    const hasLimit = p.traffic_limit > 0;
                    if (usageText) usageText.style.display = hasLimit ? '' : 'none';
                    if (barWrap) barWrap.style.display = hasLimit ? '' : 'none';
                    if (hasLimit) {
                        const total = row.querySelector('[data-stat="profile-total"]');
                        const limit = row.querySelector('[data-stat="profile-limit"]');
                        const pct = row.querySelector('[data-stat="profile-pct"]');
                        const bar = row.querySelector('[data-stat="profile-bar"]');
                        if (total) total.textContent = p.traffic_total_fmt;
                        if (limit) limit.textContent = p.traffic_limit_fmt;
                        if (pct) pct.textContent = p.usage_percent;
                        if (bar) {
                            bar.style.width = p.usage_percent + '%';
                            if (p.progress_color) bar.style.background = p.progress_color;
                        }
                    }
                });
            });
        })
        .catch(() => {});
}

function setRefresh(sec) {
    clearInterval(timer);
    clearInterval(countdown);
    localStorage.setItem('admin-refresh', sec);
    sec = parseInt(sec);
    if (sec === 0) {
        document.getElementById('refresh-countdown').textContent = '';
        return;
    }
    remaining = sec;
    updateCountdown();
    countdown = setInterval(updateCountdown, 1000);
    timer = setInterval(refreshStats, sec * 1000);
}

function updateCountdown() {
    document.getElementById('refresh-countdown').textContent = remaining + 'с';
    remaining--;
    if (remaining < 0) remaining = parseInt(document.getElementById('refresh-interval').value);
}

(function() {
    const select = document.getElementById('refresh-interval');
    if (!select) return;
    const saved = localStorage.getItem('admin-refresh') || '30';
    select.value = saved;
    setRefresh(saved);
    // CSP: inline onchange недопустим — навешиваем здесь.
    select.addEventListener('change', (e) => setRefresh(e.target.value));
})();
