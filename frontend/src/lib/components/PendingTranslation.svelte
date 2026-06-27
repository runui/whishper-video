<script>
    import {displayTranscriptionName, cancelTranslation} from "$lib/utils.js";
    export let tr;

    $: progress = tr?.translationProgress;
    $: hasProgress = progress && progress.total > 0;
</script>

<div class="alert alert-info p-3">
    <span class="loading loading-spinner loading-md"></span>
    <span class="flex-1 min-w-0">
        <p class="font-bold text-info-content text-md flex items-center space-x-2">
            <svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler icon-tabler-language-hiragana" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                <path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
                <path d="M4 5h7"></path>
                <path d="M7 4c0 4.846 0 7 .5 8"></path>
                <path d="M10 8.5c0 2.286 -2 4.5 -3.5 4.5s-2.5 -1.135 -2.5 -2c0 -2 1 -3 3 -3s5 .57 5 2.857c0 1.524 -.667 2.571 -2 3.143"></path>
                <path d="M12 20l4 -9l4 9"></path>
                <path d="M19.1 18h-6.2"></path>
            </svg>
            <span>{displayTranscriptionName(tr)}</span>
        </p>
        {#if hasProgress}
            <p class="font-mono text-sm opacity-70">
                <span>{progress.completed}/{progress.total}  ·  </span>
                <span class="flip-board">
                    {#key `${progress.currentSegment}|${progress.currentStatus}`}
                        <span class="flip-text">seg #{progress.currentSegment} {progress.currentStatus}</span>
                    {/key}
                </span>
            </p>
        {:else}
            <p class="font-mono text-info-content text-sm opacity-60">Waiting for translation...</p>
        {/if}
    </span>
    <div class="shrink-0">
        <button on:click={() => cancelTranslation(tr.id)} class="btn btn-sm">Cancel</button>
    </div>
</div>

<style>
    .flip-board {
        display: inline-block;
        perspective: 400px;
        vertical-align: bottom;
    }
    .flip-text {
        display: inline-block;
        transform-origin: 50% 0%;
        animation: flipDown 0.5s cubic-bezier(0.4, 0.0, 0.2, 1);
    }
    @keyframes flipDown {
        0% {
            transform: rotateX(-100deg);
            opacity: 0;
        }
        30% {
            opacity: 0;
        }
        50% {
            opacity: 1;
        }
        70% {
            transform: rotateX(12deg);
        }
        85% {
            transform: rotateX(-3deg);
        }
        100% {
            transform: rotateX(0deg);
        }
    }
</style>
