<script>
    import {createEventDispatcher} from 'svelte';
    import {displayTranscriptionName, cancelTranslation} from "$lib/utils.js";
    export let tr;

    const dispatch = createEventDispatcher();
    let retry = () => {
        dispatch('translate', tr);
    }
    let modalId = `errorModal-${tr.id}`;
    let showModal = () => document.getElementById(modalId)?.showModal();

    function truncateFirstLine(msg, maxLen) {
        if (!msg) return '';
        const firstLine = msg.split('\n')[0];
        return firstLine.length > maxLen ? firstLine.slice(0, maxLen) + '...' : firstLine;
    }

    function highlightError(msg) {
        if (!msg) return '';
        return msg
            .replace(/(error|failed|invalid|timeout|refused|cannot|unavailable)/gi, '<span class="text-error font-bold">$1</span>')
            .replace(/(\d{2,})/g, '<span class="text-accent">$1</span>')
            .replace(/(https?:\/\/[^\s]+)/g, '<span class="text-info underline">$1</span>')
            .replace(/(`[^`]+`)/g, '<span class="badge badge-outline badge-sm font-mono">$1</span>')
            .replace(/(\b\d{3}\b)/g, '<span class="text-accent">$1</span>');
    }
</script>

<div class="alert alert-warning p-3 cursor-pointer text-left" on:click={showModal}>
    <svg xmlns="http://www.w3.org/2000/svg" class="stroke-current shrink-0 h-6 w-6" fill="none" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.964-.833-2.732 0L4.082 16.5c-.77.833.192 2.5 1.732 2.5z" /></svg>
    <span class="flex-1 min-w-0 tooltip text-left" data-tip={tr.translationError || 'No error details'}>
        <p class="font-bold text-info-content text-md">{displayTranscriptionName(tr)}</p>
        <p class="font-mono text-info-content text-sm opacity-70">Translation failed{tr.translationError ? ': ' + truncateFirstLine(tr.translationError, 60) : ''}</p>
    </span>
    <div class="flex items-center justify-center flex-wrap space-x-2 shrink-0" on:click|stopPropagation>
        <button on:click={retry} class="btn btn-sm btn-primary">
            <span class="tooltip flex items-center justify-center" data-tip="Retry translation">
                <svg xmlns="http://www.w3.org/2000/svg" class="w-4 h-4" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                    <path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
                    <path d="M4 5h7"></path>
                    <path d="M7 4c0 4.846 0 7 .5 8"></path>
                    <path d="M10 8.5c0 2.286 -2 4.5 -3.5 4.5s-2.5 -1.135 -2.5 -2c0 -2 1 -3 3 -3s5 .57 5 2.857c0 1.524 -.667 2.571 -2 3.143"></path>
                    <path d="M12 20l4 -9l4 9"></path>
                    <path d="M19.1 18h-6.2"></path>
                </svg>
            </span>
        </button>
        <button on:click={() => cancelTranslation(tr.id)} class="btn btn-sm">
            <span class="tooltip flex items-center justify-center" data-tip="Cancel translation">
                <svg xmlns="http://www.w3.org/2000/svg" class="w-4 h-4" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                    <path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
                    <path d="M18 6l-12 12"></path>
                    <path d="M6 6l12 12"></path>
                </svg>
            </span>
        </button>
    </div>
</div>

<dialog id={modalId} class="modal">
    <form method="dialog" class="modal-box max-w-2xl text-left">
        <button class="absolute btn btn-sm btn-circle btn-ghost right-2 top-2">✕</button>
        <h3 class="text-lg font-bold flex items-center gap-2">
            <svg xmlns="http://www.w3.org/2000/svg" class="w-5 h-5 text-warning" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round"><path stroke="none" d="M0 0h24v24H0z" fill="none"/><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.964-.833-2.732 0L4.082 16.5c-.77.833.192 2.5 1.732 2.5z"/></svg>
            Translation Error
        </h3>
        <p class="mb-2 text-sm opacity-60">{displayTranscriptionName(tr)}</p>
        <div class="divider my-2"></div>
        {#if tr.translationError}
            <div class="overflow-y-auto max-h-96 rounded-lg p-4 bg-base-300">
                <pre class="text-sm font-mono whitespace-pre-wrap leading-relaxed">{@html highlightError(tr.translationError)}</pre>
            </div>
        {:else}
            <p class="text-sm opacity-60">No error details available.</p>
        {/if}
        <div class="modal-action">
            <button class="btn btn-sm">Close</button>
        </div>
    </form>
    <form method="dialog" class="modal-backdrop">
        <button>close</button>
    </form>
</dialog>
