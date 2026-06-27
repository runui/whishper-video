<script>
    import {displayTranscriptionName} from "$lib/utils.js";
    export let tr;
</script>

<div class="alert alert-info p-3">
    <span class="loading loading-ring loading-md"></span>
    <span class="flex-1 min-w-0">
        <p class="font-bold text-info-content text-md">{displayTranscriptionName(tr)}</p>
        <p class="font-mono text-info-content text-sm opacity-60">Waiting for transcription...</p>
    </span>
</div>
