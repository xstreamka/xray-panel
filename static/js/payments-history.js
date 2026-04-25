function setFilter(btn) {
    const kind = btn.dataset.filter;

    document.querySelectorAll('[data-filter]').forEach(b => {
        if (b === btn) {
            b.className = 'btn btn-sm btn-primary';
            b.style.background = '';
            b.style.color = '';
        } else {
            b.className = 'btn btn-sm';
            b.style.background = '#334155';
            b.style.color = '#e2e8f0';
        }
    });

    document.querySelectorAll('tbody tr').forEach(row => {
        const k = row.dataset.kind;
        row.style.display = (kind === 'all' || k === kind) ? '' : 'none';
    });
}
