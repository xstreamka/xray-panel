// Клиентский поиск по списку пользователей /admin.
// Никаких запросов на сервер: просто фильтрация уже отрендеренных строк по
// атрибуту data-search (логин + email). Сохраняем последний запрос в
// localStorage, чтобы после обновления страницы фильтр не сбрасывался.

(function () {
    const input = document.getElementById('user-search');
    const countEl = document.getElementById('user-search-count');
    const rows = document.querySelectorAll('[data-search]');
    if (!input || rows.length === 0) return;

    const total = rows.length;

    function apply(q) {
        q = q.trim().toLowerCase();
        let visible = 0;
        rows.forEach(r => {
            const hay = r.getAttribute('data-search').toLowerCase();
            const match = !q || hay.includes(q);
            r.style.display = match ? '' : 'none';
            if (match) visible++;
        });
        if (countEl) {
            countEl.textContent = q ? `${visible} / ${total}` : `всего ${total}`;
        }
        try { localStorage.setItem('admin-search-q', q); } catch (e) { /* ignore */ }
    }

    const saved = (function () {
        try { return localStorage.getItem('admin-search-q') || ''; }
        catch (e) { return ''; }
    })();

    input.value = saved;
    apply(saved);

    input.addEventListener('input', e => apply(e.target.value));
})();
