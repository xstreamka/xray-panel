// График трафика на /dashboard. Тянет агрегированные точки из
// /dashboard/traffic?range=... и рисует line-chart (up + down).
// Переключение периода — клик по кнопкам в #traffic-range.

(function () {
    const canvas = document.getElementById('traffic-chart');
    const empty = document.getElementById('traffic-empty');
    const rangeBox = document.getElementById('traffic-range');
    if (!canvas || !rangeBox || typeof Chart === 'undefined') return;

    let chart = null;
    let currentRange = '24h';

    function formatBytes(b) {
        if (b < 1024) return b + ' B';
        const units = ['KB', 'MB', 'GB', 'TB'];
        let v = b / 1024, i = 0;
        while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
        return v.toFixed(v >= 10 ? 0 : 1) + ' ' + units[i];
    }

    // Label для оси X зависит от периода: на 24h показываем час:минуту,
    // на 7d — день+час, на 30d/90d — просто дату.
    function labelFormatter(range) {
        return function (iso) {
            const d = new Date(iso);
            const pad = (n) => String(n).padStart(2, '0');
            if (range === '24h') {
                return pad(d.getHours()) + ':' + pad(d.getMinutes());
            }
            if (range === '7d') {
                return pad(d.getDate()) + '.' + pad(d.getMonth() + 1) + ' ' + pad(d.getHours()) + ':00';
            }
            return pad(d.getDate()) + '.' + pad(d.getMonth() + 1);
        };
    }

    function render(data) {
        const points = data.points || [];
        if (points.length === 0) {
            if (chart) { chart.destroy(); chart = null; }
            empty.style.display = 'flex';
            return;
        }
        empty.style.display = 'none';

        const fmt = labelFormatter(data.range);
        const labels = points.map(p => fmt(p.t));
        const up = points.map(p => p.up);
        const down = points.map(p => p.down);

        if (chart) chart.destroy();
        chart = new Chart(canvas, {
            type: 'line',
            data: {
                labels: labels,
                datasets: [
                    {
                        label: 'Входящий',
                        data: down,
                        borderColor: '#3b82f6',
                        backgroundColor: 'rgba(59, 130, 246, 0.15)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: points.length > 60 ? 0 : 2,
                        borderWidth: 2,
                    },
                    {
                        label: 'Исходящий',
                        data: up,
                        borderColor: '#fcd34d',
                        backgroundColor: 'rgba(252, 211, 77, 0.15)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: points.length > 60 ? 0 : 2,
                        borderWidth: 2,
                    },
                ],
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                interaction: { mode: 'index', intersect: false },
                plugins: {
                    legend: {
                        labels: {
                            color: '#e2e8f0',
                            boxWidth: 14,
                            boxHeight: 14,
                            // По умолчанию Chart.js заливает кубик легенды
                            // backgroundColor'ом датасета — у нас он намеренно
                            // прозрачный (0.15) для мягкой area-заливки графика,
                            // поэтому на тёмном фоне легенда выглядит как пустой
                            // контур. Подменяем fillStyle на насыщенный borderColor.
                            generateLabels: function (chart) {
                                const base = Chart.defaults.plugins.legend.labels.generateLabels(chart);
                                base.forEach((item, i) => {
                                    const ds = chart.data.datasets[i];
                                    if (ds && ds.borderColor) {
                                        item.fillStyle = ds.borderColor;
                                        item.strokeStyle = ds.borderColor;
                                    }
                                });
                                return base;
                            },
                        },
                    },
                    tooltip: {
                        callbacks: {
                            label: (ctx) => ctx.dataset.label + ': ' + formatBytes(ctx.parsed.y),
                        },
                    },
                },
                scales: {
                    x: {
                        ticks: { color: '#94a3b8', maxRotation: 0, autoSkip: true, maxTicksLimit: 10 },
                        grid: { color: 'rgba(148, 163, 184, 0.1)' },
                    },
                    y: {
                        ticks: {
                            color: '#94a3b8',
                            callback: (v) => formatBytes(v),
                        },
                        grid: { color: 'rgba(148, 163, 184, 0.1)' },
                        beginAtZero: true,
                    },
                },
            },
        });
    }

    function load(range) {
        currentRange = range;
        // Подсветка активной кнопки.
        rangeBox.querySelectorAll('button[data-range]').forEach((b) => {
            const active = b.dataset.range === range;
            b.classList.toggle('btn-primary', active);
            b.style.background = active ? '' : '#334155';
            b.style.color = active ? '' : '#e2e8f0';
        });

        fetch('/dashboard/traffic?range=' + encodeURIComponent(range))
            .then((r) => r.ok ? r.json() : Promise.reject(r.status))
            .then(render)
            .catch((err) => console.error('traffic chart load:', err));
    }

    rangeBox.addEventListener('click', (e) => {
        const btn = e.target.closest('button[data-range]');
        if (!btn) return;
        load(btn.dataset.range);
    });

    load(currentRange);
})();
