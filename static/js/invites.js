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
