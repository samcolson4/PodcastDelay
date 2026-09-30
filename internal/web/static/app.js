// Tiny helpers layered on htmx: toasts and the copy-to-clipboard buttons.
(function () {
  var toastEl, toastTimer;

  function toast(msg, kind) {
    toastEl = toastEl || document.getElementById('toast');
    if (!toastEl) return;
    toastEl.textContent = msg;
    toastEl.className = 'show' + (kind ? ' ' + kind : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toastEl.className = ''; }, 2500);
  }

  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    // Plain-HTTP fallback (clipboard API needs a secure context).
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) {}
      document.body.removeChild(ta);
      ok ? resolve() : reject(new Error('copy failed'));
    });
  }

  // Delegated so it keeps working after htmx swaps the table body.
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy]');
    if (!btn) return;
    copyText(btn.dataset.copy).then(function () {
      toast('Feed URL copied to clipboard');
      var menu = btn.closest('details');
      if (menu) menu.removeAttribute('open');
    }, function () {
      toast('Could not copy — select the URL and copy it manually', 'error');
    });
  });

  // Schedule page: hide already-released episodes (remembered per browser).
  var hideBox = document.getElementById('hide-released');
  var epTable = document.getElementById('episodes');
  if (hideBox && epTable) {
    try { hideBox.checked = localStorage.getItem('hideReleased') === '1'; } catch (e) {}
    var apply = function () { epTable.classList.toggle('hide-released', hideBox.checked); };
    hideBox.addEventListener('change', function () {
      apply();
      try { localStorage.setItem('hideReleased', hideBox.checked ? '1' : '0'); } catch (e) {}
    });
    apply();
  }

  // Close any open actions menu when clicking elsewhere.
  document.addEventListener('click', function (e) {
    document.querySelectorAll('.actions details[open]').forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute('open');
    });
  });

  // Failed htmx actions (e.g. a refresh that can't reach the source) → toast.
  document.body.addEventListener('htmx:responseError', function (e) {
    var text = (e.detail.xhr.responseText || '').trim();
    toast(text || 'Request failed (' + e.detail.xhr.status + ')', 'error');
  });
  document.body.addEventListener('htmx:sendError', function () {
    toast('Network error', 'error');
  });
})();
