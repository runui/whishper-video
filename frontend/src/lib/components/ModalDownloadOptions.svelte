<script>
    import {downloadSRT, downloadTXT, downloadJSON, downloadVTT, generateSRT, generateVTT, generateTXT, generateJSON, CLIENT_API_HOST} from '$lib/utils';
    import toast from 'svelte-french-toast';
    export let tr;

    let subtitleFormat = "srt";
    let selectedText = "main:original";

    $: textOptions = tr ? getTextOptions(tr) : [];
    $: if (textOptions.length > 0 && !textOptions.some(option => option.value == selectedText)) {
        selectedText = textOptions[0].value;
    }

    function getMediaTitle(transcription) {
        return transcription.fileName.includes("_WHSHPR_") ? transcription.fileName.split("_WHSHPR_")[1] : transcription.fileName;
    }

    function getTrackLabel(track) {
        const title = track.title ? ` - ${track.title}` : '';
        return `Subtitle ${track.index} (${track.language || 'unknown'}${title})`;
    }

    function engineIcon(engine) {
        return engine === 'llm' ? '🧠' : '🤖';
    }

    function getTextOptions(transcription) {
        const options = [];
        if (!transcription.skipWhisper || transcription.result?.segments?.length > 0) {
            options.push({ value: 'main:original', label: `✅ Transcription (${transcription.result.language})`, result: transcription.result });
        }
        const sortedTranslations = [...(transcription.translations || [])].sort((a, b) => a.targetLanguage.localeCompare(b.targetLanguage));
        for (const translation of sortedTranslations) {
            options.push({ value: `main:${translation.targetLanguage}`, label: `${engineIcon(translation.engine)} ${translation.targetLanguage}`, result: translation.result });
        }
        for (const track of transcription.subtitleTracks || []) {
            options.push({ value: `subtitle:${track.id}:original`, label: `✅ ${getTrackLabel(track)}`, result: track.result });
            const sortedTrackTranslations = [...(track.translations || [])].sort((a, b) => a.targetLanguage.localeCompare(b.targetLanguage));
            for (const translation of sortedTrackTranslations) {
                options.push({ value: `subtitle:${track.id}:${translation.targetLanguage}`, label: `${getTrackLabel(track)} ${engineIcon(translation.engine)} ${translation.targetLanguage}`, result: translation.result });
            }
        }
        return options;
    }

    function selectedOption() {
        return textOptions.find(option => option.value == selectedText) || textOptions[0];
    }

    function getLanguage() {
        const parts = selectedText.split(':');
        const lang = parts[parts.length - 1];
        if (lang === 'original') {
            const option = selectedOption();
            return option?.result?.language || tr?.result?.language || 'original';
        }
        return lang;
    }

    function downloadSubtitle() {
        const option = selectedOption();
        const segments = option?.result?.segments || [];
        const text = option?.result?.text || "";
        const title = getMediaTitle(tr);
        const lang = getLanguage();
        
        if (!option || segments.length == 0 || text == "") {
            toast.error("No data available for download");
            return;
        }

        if (subtitleFormat == "srt") {
            downloadSRT(segments, title, lang);
        } else if (subtitleFormat == "vtt") {
            downloadVTT(segments, title, lang);
        } else if (subtitleFormat == "json") {
            downloadJSON(option.result, title, lang);
        } else if (subtitleFormat == "txt") {
            downloadTXT(text, title, lang);
        }
    }

    function downloadMedia() {
        let link = document.createElement('a');
        link.href = `${CLIENT_API_HOST}/api/video/${tr.fileName}`;
        link.target = "_blank";
        link.download = tr.fileName.split('_WHSHPR_')[1]+".mp4";
        link.click();
    }

    async function copyText() {
        const text = selectedOption()?.result?.text || "";
        try {
            await navigator.clipboard.writeText(text);
            toast.success('Text copied to clipboard');
        } catch (err) {
            console.error('Error in copying text: ', err);
        }
    }

    function generateContent() {
        const option = selectedOption();
        const segments = option?.result?.segments || [];
        const text = option?.result?.text || "";
        if (!option || segments.length == 0 || text == "") return null;
        const title = getMediaTitle(tr);
        switch (subtitleFormat) {
            case "srt": return generateSRT(segments);
            case "vtt": return generateVTT(segments);
            case "json": return generateJSON(option.result);
            case "txt": return generateTXT(text);
        }
        return null;
    }

    async function writeToFile() {
        const content = generateContent();
        if (!content) {
            toast.error("No data available to write");
            return;
        }

        const option = selectedOption();
        const lang = getLanguage();
        const title = getMediaTitle(tr);
        const filename = `${title}_${lang}.${subtitleFormat}`;

        async function doWrite(overwrite) {
            const res = await fetch(`${CLIENT_API_HOST}/api/subtitles/${tr.id}/write`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ format: subtitleFormat, content, filename, overwrite })
            });
            if (res.ok) {
                const data = await res.json();
                const name = data.path.split('/').pop() || data.path;
                toast.success(`Written to ${name}`);
                return true;
            }
            if (res.status === 409 && !overwrite) {
                return "conflict";
            }
            const err = await res.text().catch(() => "Unknown error");
            toast.error(err.length > 80 ? `Failed to write: ${err.slice(0, 80)}...` : `Failed to write: ${err}`);
            return false;
        }

        let result = await doWrite(false);
        if (result === "conflict") {
            const ok = confirm("File already exists. Overwrite?");
            if (!ok) {
                toast("Cancelled", { icon: "👋" });
                return;
            }
            result = await doWrite(true);
        }
    }
</script>

<dialog id="modalDownloadOptions" class="modal">
    <form method="dialog" class="modal-box flex flex-col items-center justify-center" style="overflow:hidden">
        <button class="btn btn-sm btn-circle btn-ghost absolute right-2 top-2">✕</button>
        {#if tr}
        <h1 class="text-center font-bold mt-2 pb-2">Download Options</h1>

        <div class="flex flex-row flex-wrap gap-4 justify-center">
            <div class="form-control min-w-0">
                <label for="format" class="label">
                    <span class="label-text font-bold">File Format</span>
                </label>
                <select bind:value={subtitleFormat} name="format" class="select select-bordered w-full">
                    <option value="srt">SRT</option>
                    <option value="vtt">VTT</option>
                    <option value="json">JSON</option>
                    <option value="txt">TXT</option>
                </select>
            </div>
    
            <div class="form-control min-w-0">
                <label for="language" class="label">
                    <span class="label-text font-bold">Text Source</span>
                </label>
                <select bind:value={selectedText} name="language" class="select select-bordered w-full">
                    {#each textOptions as option}
                        <option value={option.value}>{option.label}</option>
                    {/each}
                </select>
            </div>
        </div>

        <div class="space-x-2 mt-8">
            <span class="tooltip" data-tip="Download selected text as {subtitleFormat}.">
                <button on:click={downloadSubtitle} class="btn btn-sm btn-success">
                    <svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler icon-tabler-download" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                        <path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
                        <path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2 -2v-2"></path>
                        <path d="M7 11l5 5l5 -5"></path>
                        <path d="M12 4l0 12"></path>
                    </svg>
                    <span>File</span>
                </button>
            </span>
            <span class="tooltip" data-tip="Copy selected text.">
                <button on:click={copyText} class="btn btn-sm btn-info">
                    <svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler icon-tabler-copy" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                        <path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
                        <path d="M8 8m0 2a2 2 0 0 1 2 -2h8a2 2 0 0 1 2 2v8a2 2 0 0 1 -2 2h-8a2 2 0 0 1 -2 -2z"></path>
                        <path d="M16 8v-2a2 2 0 0 0 -2 -2h-8a2 2 0 0 0 -2 2v8a2 2 0 0 0 2 2h2"></path>
                     </svg>
                     <span>Copy</span>
                </button>
            </span>
            <span class="tooltip" data-tip="Download source media">
                <button on:click={downloadMedia} class="btn btn-sm btn-error">
                    <svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler icon-tabler-file-download" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                        <path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
                        <path d="M14 3v4a1 1 0 0 0 1 1h4"></path>
                        <path d="M17 21h-10a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h7l5 5v11a2 2 0 0 1 -2 2z"></path>
                        <path d="M12 17v-6"></path>
                        <path d="M9.5 14.5l2.5 2.5l2.5 -2.5"></path>
                    </svg>
                    <span>Media</span>
                </button>
            </span>
            {#if tr.localPath}
                <span class="tooltip" data-tip="Write subtitle next to source file">
                    <button on:click={writeToFile} class="btn btn-sm btn-primary">
                        <svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler icon-tabler-file-download" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
                            <path stroke="none" d="M0 0h24v24H0z" fill="none"/>
                            <path d="M14 3v4a1 1 0 0 0 1 1h4"/>
                            <path d="M17 21h-10a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h7l5 5v11a2 2 0 0 1 -2 2z"/>
                            <path d="M12 17v-6"/>
                            <path d="M9.5 14.5l2.5 2.5l2.5 -2.5"/>
                        </svg>
                        <span>Save</span>
                    </button>
                </span>
            {/if}
        </div>
        {:else}
            <div class="flex items-center justify-center w-screen h-screen">
                <h1>
                    <span class="loading loading-bars loading-lg" />
                </h1>
            </div>
        {/if}
    </form>
    <form method="dialog" class="modal-backdrop">
        <button>close</button>
    </form>
</dialog>
