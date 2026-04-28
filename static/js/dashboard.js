function copyURI(btn) {
    navigator.clipboard.writeText(btn.dataset.uri).then(() => {
        const orig = btn.textContent;
        btn.textContent = '✓';
        setTimeout(() => btn.textContent = orig, 1500);
    });
}
function showQR(btn) {
    const modal = document.getElementById('qr-modal');
    const container = document.getElementById('qr-canvas');
    container.innerHTML = '';
    new QRCode(container, {
        text: btn.dataset.uri,
        width: 280,
        height: 280,
        correctLevel: QRCode.CorrectLevel.L
    });
    modal.style.display = 'flex';
}

// Делегированный диспетчер действий на карточках профилей.
// Inline onclick недопустим из-за CSP — вешаем на data-action.
document.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-action]');
    if (!btn) return;
    switch (btn.dataset.action) {
        case 'copy-uri':
            copyURI(btn);
            break;
        case 'show-qr':
            showQR(btn);
            break;
        case 'show-profile-chart':
            if (typeof window.showProfileChart === 'function') {
                window.showProfileChart(btn);
            }
            break;
    }
});

// Любая мутация (toggle/delete/reset/limit) MTProto-профиля влечёт за собой
// рестарт контейнера mtprotoproxy на бэке (~5–10 сек), всё это время запрос висит.
// Чтобы юзер не дёргал кнопки повторно, при submit вешаем полупрозрачный оверлей
// со спиннером и дизейблим все интерактивные элементы карточки. После редиректа
// на /dashboard страница перезагрузится — оверлей исчезнет вместе с DOM.
function lockProfileCard(card, message) {
    if (card.dataset.locked === '1') return;
    card.dataset.locked = '1';
    card.style.position = card.style.position || 'relative';
    // ВАЖНО: <input> мы НЕ трогаем — браузер сериализует FormData уже после
    // submit-события, и любой disabled-инпут (включая hidden CSRF и обычные
    // number/text) выпадает из тела запроса. Это ломало и CSRF (403 mismatch),
    // и значение limit_gb=0 (на бэке прилетал пустой string → ParseFloat fail).
    // Оверлей сверху всё равно блокирует все клики, поэтому визуальный дизейбл
    // инпутов нам не нужен.
    card.querySelectorAll('button, select, a').forEach(el => {
        if (el.tagName === 'A') {
            el.style.pointerEvents = 'none';
            el.style.opacity = '0.5';
        } else {
            el.disabled = true;
        }
    });
    const overlay = document.createElement('div');
    overlay.dataset.lockOverlay = '1';
    overlay.style.cssText = [
        'position:absolute', 'inset:0', 'background:rgba(15,23,42,0.65)',
        'display:flex', 'align-items:center', 'justify-content:center',
        'gap:0.6rem', 'border-radius:inherit', 'z-index:10',
        'color:#e2e8f0', 'font-size:0.9rem',
    ].join(';');
    overlay.innerHTML =
        '<span class="profile-spinner" style="display:inline-block;width:18px;height:18px;'
        + 'border:2px solid rgba(226,232,240,0.3);border-top-color:#e2e8f0;'
        + 'border-radius:50%;animation:profile-spin 0.8s linear infinite;"></span>'
        + '<span>' + message + '</span>';
    card.appendChild(overlay);
}

// Один общий keyframes — добавим в head, если ещё нет.
(function ensureSpinnerKeyframes() {
    if (document.getElementById('profile-spinner-style')) return;
    const style = document.createElement('style');
    style.id = 'profile-spinner-style';
    style.textContent = '@keyframes profile-spin { to { transform: rotate(360deg); } }';
    document.head.appendChild(style);
})();

document.addEventListener('submit', (e) => {
    const form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    if (e.defaultPrevented) return; // confirm в csp-shim уже отменил
    const card = form.closest('[data-profile-kind="mtproto"]');
    if (!card) return;
    // POST на /dashboard/mtproto/{id}/... — все они вызывают Sync/SyncForceDisconnect.
    // Sync (SIGUSR2) укладывается в секунду, рестарт — до 10. Всё равно показываем
    // оверлей: для быстрых операций он мелькнёт незаметно перед reload'ом.
    lockProfileCard(card, 'Операция выполняется');
}, false);

function refreshStats() {
    fetch('/dashboard/stats')
        .then(r => r.json())
        .then(data => {
            // Баланс (общий)
            const balanceEl = document.getElementById('balance-display');
            if (balanceEl && data.balance_fmt) balanceEl.textContent = data.balance_fmt;

            // Подписка: название, статус, срок, дни
            const tariffLabelEl = document.querySelector('[data-stat="tariff-label"]');
            const subBadgeEl = document.querySelector('[data-stat="sub-badge"]');
            if (tariffLabelEl) {
                tariffLabelEl.textContent = data.tariff_label || '—';
                tariffLabelEl.style.color = data.tariff_label ? '' : '#94a3b8';
            }
            if (subBadgeEl) {
                if (data.has_active_subscription) {
                    subBadgeEl.className = 'badge badge-green';
                    subBadgeEl.textContent = 'активна';
                } else if (data.tariff_label) {
                    subBadgeEl.className = 'badge badge-red';
                    subBadgeEl.textContent = 'истёк';
                } else {
                    subBadgeEl.className = 'badge badge-gray';
                    subBadgeEl.textContent = 'нет подписки';
                }
            }

            const expiresEl = document.querySelector('[data-stat="expires-at"]');
            if (expiresEl && data.tariff_expires_at) {
                const d = new Date(data.tariff_expires_at);
                expiresEl.textContent = d.toLocaleDateString('ru-RU',
                    { day: '2-digit', month: '2-digit', year: 'numeric' });
            }
            const daysLine = document.querySelector('[data-stat="days-left-line"]');
            if (daysLine) {
                const strong = daysLine.querySelector('strong');
                if (strong) {
                    strong.textContent = (data.days_left || 0) + ' дн.';
                    strong.style.color = data.days_left < 3 ? '#f59e0b' : '#e2e8f0';
                }
            }

            // Прогресс базового трафика
            const baseUsedEl = document.querySelector('[data-stat="base-used"]');
            const baseLimitEl = document.querySelector('[data-stat="base-limit"]');
            const baseBar = document.querySelector('[data-stat="base-bar"]');
            if (baseUsedEl) baseUsedEl.textContent = data.base_used_fmt;
            if (baseLimitEl) baseLimitEl.textContent = data.base_limit_fmt;
            if (baseBar && data.base_limit > 0) {
                const used = Math.min(data.base_used, data.base_limit);
                const pct = Math.round(used / data.base_limit * 100);
                baseBar.style.width = pct + '%';
                baseBar.style.background = pct >= 90 ? '#ef4444' : pct >= 70 ? '#f59e0b' : '#22c55e';
            }

            // Прогресс-бар докупленного трафика
            const extraRow = document.querySelector('[data-stat="extra-row"]');
            const extraUsedEl = document.querySelector('[data-stat="extra-used"]');
            const extraGrantedEl = document.querySelector('[data-stat="extra-granted"]');
            const extraBar = document.querySelector('[data-stat="extra-bar"]');
            if (extraRow) extraRow.style.display = data.extra_granted > 0 ? '' : 'none';
            if (extraUsedEl) extraUsedEl.textContent = data.extra_used_fmt;
            if (extraGrantedEl) extraGrantedEl.textContent = data.extra_granted_fmt;
            if (extraBar && data.extra_granted > 0) {
                const capped = Math.min(data.extra_used || 0, data.extra_granted);
                const pct = Math.round(capped / data.extra_granted * 100);
                extraBar.style.width = pct + '%';
                extraBar.style.background = pct >= 90 ? '#ef4444' : pct >= 70 ? '#f59e0b' : '#fcd34d';
            }

            // Extra / Frozen блоки
            const extraBlock = document.querySelector('[data-stat="extra-block"]');
            const extraEl = document.querySelector('[data-stat="extra-balance"]');
            if (extraBlock) extraBlock.style.display = data.extra_balance > 0 ? '' : 'none';
            if (extraEl) extraEl.textContent = data.extra_balance_fmt;

            const frozenBlock = document.querySelector('[data-stat="frozen-block"]');
            const frozenEl = document.querySelector('[data-stat="frozen-balance"]');
            if (frozenBlock) frozenBlock.style.display = data.frozen_balance > 0 ? '' : 'none';
            if (frozenEl) frozenEl.textContent = data.frozen_balance_fmt;

            // Запрещаем создавать профили и включать выключенные, если баланс 0:
            // иначе профиль на пару секунд оживёт и тут же будет убит коллектором.
            const noBalance = (data.balance || 0) <= 0;
            const blockedTitle = 'Нет доступного трафика. Оплатите тариф.';
            document.querySelectorAll('[data-stat="create-profile-btn"], [data-stat="activate-btn"]').forEach(btn => {
                btn.disabled = noBalance;
                if (noBalance) {
                    btn.dataset.origTitle = btn.dataset.origTitle || btn.title;
                    btn.title = blockedTitle;
                } else if (btn.dataset.origTitle !== undefined) {
                    btn.title = btn.dataset.origTitle;
                }
            });

            (data.profiles || []).forEach(s => {
                const kind = s.kind || 'vpn';
                const card = document.querySelector(`[data-profile-kind="${kind}"][data-profile-id="${s.id}"]`);
                if (!card) return;

                const up = card.querySelector('[data-stat="up"]');
                const down = card.querySelector('[data-stat="down"]');
                const total = card.querySelector('[data-stat="total"]');
                if (up) up.textContent = '↑ ' + s.traffic_up_fmt;
                if (down) down.textContent = '↓ ' + s.traffic_down_fmt;
                if (total) total.textContent = 'Σ ' + s.traffic_total_fmt;

                const usageText = card.querySelector('[data-stat="usage-text"]');
                const usagePct = card.querySelector('[data-stat="usage-pct"]');
                const bar = card.querySelector('[data-stat="progress-bar"]');
                if (usageText && s.limit_fmt) usageText.textContent = s.traffic_total_fmt + ' / ' + s.limit_fmt;
                if (usagePct) usagePct.textContent = s.usage_percent + '%';
                if (bar) {
                    bar.style.width = s.usage_percent + '%';
                    if (s.progress_color) bar.style.background = s.progress_color;
                }

                const statusEl = card.querySelector('[data-stat="status"]');
                const ipsEl = card.querySelector('[data-stat="ips"]');
                if (statusEl) {
                    statusEl.style.background = '';
                    statusEl.style.color = '';
                    if (!s.is_active) {
                        if (s.is_expired) {
                            statusEl.className = 'badge badge-red';
                            statusEl.textContent = 'expired';
                        } else if (s.is_over_limit) {
                            statusEl.className = 'badge badge-red';
                            statusEl.textContent = 'лимит';
                        } else {
                            statusEl.className = 'badge badge-red';
                            statusEl.textContent = 'disabled';
                        }
                    } else if (s.is_online) {
                        statusEl.className = 'badge badge-green';
                        statusEl.textContent = '● online';
                    } else {
                        statusEl.className = 'badge badge-blue';
                        statusEl.textContent = 'active';
                    }
                }
                card.style.opacity = s.is_active ? '' : '0.6';
                if (ipsEl) {
                    const ips = s.online_ips || [];
                    if (s.is_active && s.is_online && ips.length > 0) {
                        ipsEl.innerHTML = ips.map(ip => '<span class="badge badge-blue">' + ip + '</span> ').join('');
                    } else if (kind === 'mtproto' && s.is_active && s.is_online) {
                        ipsEl.innerHTML = '<span class="badge badge-blue">' + (s.current_conns || 0) + ' conn</span>';
                    } else {
                        ipsEl.innerHTML = '';
                    }
                }
            });
        })
        .catch(() => {});
}
setInterval(refreshStats, 5000);
