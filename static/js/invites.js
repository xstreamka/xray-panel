// Копирование ссылки-инвайта в буфер обмена по клику на 📋.
// Fallback: prompt() — если navigator.clipboard недоступен (http, старый браузер).
document.querySelectorAll('[data-invite-link]').forEach(btn => {
    btn.addEventListener('click', async () => {
        const link = btn.getAttribute('data-invite-link');
        try {
            await navigator.clipboard.writeText(link);
            const old = btn.textContent;
            btn.textContent = '✓';
            setTimeout(() => { btn.textContent = old; }, 1200);
        } catch (e) {
            prompt('Скопируйте ссылку:', link);
        }
    });
});

// Редактирование заметки инвайта: prompt → подставляем в скрытую форму
// /admin/invites/{id}/note → submit. Так не нужен отдельный AJAX-эндпоинт
// и форма уже несёт CSRF-токен из шаблона.
document.querySelectorAll('[data-edit-invite-note]').forEach(btn => {
    btn.addEventListener('click', () => {
        const id = btn.getAttribute('data-edit-invite-note');
        const current = btn.getAttribute('data-current-note') || '';
        const next = prompt('Заметка (до 255 символов, пусто = очистить):', current);
        if (next === null) return;          // отмена
        if (next === current) return;       // без изменений — не дёргаем POST
        const form = document.getElementById('note-form-' + id);
        if (!form) return;
        form.elements['note'].value = next;
        form.submit();
    });
});
