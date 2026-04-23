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

                (u.profiles || []).forEach(p => {
                    const row = card.querySelector(`[data-profile-id="${p.id}"]`);
                    if (!row) return;

                    const status = row.querySelector('[data-stat="profile-status"]');
                    const ipsEl = row.querySelector('[data-stat="profile-ips"]');
                    const traffic = row.querySelector('[data-stat="profile-traffic"]');

                    if (status) {
                        if (status.textContent.trim() === 'disabled') {
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
                        if (p.is_online && ips.length > 0) {
                            ipsEl.innerHTML = ips.map(ip => '<span class="badge badge-blue">' + ip + '</span> ').join('');
                        } else {
                            ipsEl.innerHTML = '';
                        }
                    }

                    if (traffic) traffic.textContent = '↑' + p.traffic_up_fmt + ' ↓' + p.traffic_down_fmt;
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
    const saved = localStorage.getItem('admin-refresh') || '30';
    document.getElementById('refresh-interval').value = saved;
    setRefresh(saved);
})();
