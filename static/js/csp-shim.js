// CSP-shim: общие делегированные обработчики, заменяющие inline onclick/onsubmit.
// Подключён в base.html — работает на всех страницах. Логика, специфичная для
// конкретного экрана (например, copyURI на dashboard), остаётся в её JS-файле.

// data-confirm="..." на <form> или <button>: показывает confirm перед сабмитом/действием.
// На <form> — отменяет submit, если юзер нажал Отмена.
// На <button type="submit"> внутри формы — то же самое (отменяет submit формы).
document.addEventListener('submit', (e) => {
    const form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    const msg = form.dataset.confirm;
    if (msg && !confirm(msg)) {
        e.preventDefault();
    }
}, true);

document.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-confirm]');
    if (btn && !confirm(btn.dataset.confirm)) {
        e.preventDefault();
        e.stopPropagation();
    }
}, true);

// data-close-modal="<id>" — кнопка-крестик закрывает модалку по id.
// data-modal-bg — клик по самому фону модалки закрывает её (но не клик по контенту).
document.addEventListener('click', (e) => {
    const closeBtn = e.target.closest('[data-close-modal]');
    if (closeBtn) {
        const id = closeBtn.dataset.closeModal;
        const m = id ? document.getElementById(id) : closeBtn.closest('[data-modal-bg]');
        if (m) m.style.display = 'none';
        return;
    }
    const bg = e.target.closest('[data-modal-bg]');
    if (bg && e.target === bg) {
        bg.style.display = 'none';
    }
});
