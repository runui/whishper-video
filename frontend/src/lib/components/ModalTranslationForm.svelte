<script>
    import { onMount } from 'svelte';
    import toast from 'svelte-french-toast';
    import { CLIENT_API_HOST } from '$lib/utils';
	import { env } from '$env/dynamic/public';
    export let tr;

    let targetLanguage = null;
    let selectedSource = 'main';

    let availableLanguages = [];
    const libreTranslateLanguageAliases = {
        zh: 'zh-Hans'
    };

    $: sourceOptions = tr ? getSourceOptions(tr) : [];
    $: if (sourceOptions.length > 0 && !sourceOptions.some(option => option.value == selectedSource)) {
        selectedSource = sourceOptions[0].value;
    }
    $: selectedSourceOption = sourceOptions.find(option => option.value == selectedSource) || sourceOptions[0];
    $: sourceLanguage = selectedSourceOption ? libreTranslateLanguageAliases[selectedSourceOption.language] || selectedSourceOption.language : null;
    $: targetLanguages = getTargetLanguages(sourceLanguage, availableLanguages);
    $: if (targetLanguage && targetLanguages.length > 0 && !targetLanguages.includes(targetLanguage)) {
        targetLanguage = null;
    }

    function getTrackLabel(track) {
        const title = track.title ? ` - ${track.title}` : '';
        return `Subtitle ${track.index} (${track.language || 'unknown'}${title})`;
    }

    function getSourceOptions(transcription) {
        const options = [];
        if (!transcription.skipWhisper || transcription.result?.segments?.length > 0) {
            options.push({ value: 'main', label: `Transcription (${transcription.result.language})`, language: transcription.result.language });
        }
        for (const track of transcription.subtitleTracks || []) {
            options.push({ value: `subtitle:${track.id}`, label: getTrackLabel(track), language: track.language || 'auto' });
        }
        return options;
    }

    function getTargetLanguages(source, languages) {
        if (!languages || languages.length == 0) {
            return [];
        }

        if (!source || source == 'auto' || source == 'und' || source == 'unknown') {
            return [...new Set(languages.flatMap(language => language.targets || []).filter(Boolean))].sort();
        }

        const sourceEntry = languages.find(language => language.code == source);
        if (sourceEntry?.targets?.length > 0) {
            return sourceEntry.targets.filter(target => target != source).sort();
        }

        return [...new Set(languages.map(language => language.code).filter(code => code && code != source))].sort();
    }

    const getAvailableLangs = () => {
        const fetchLanguages = () => {
            fetch(`${env.PUBLIC_TRANSLATION_API_HOST}/languages`)
            .then(res => res.json())
            .then(data => {
                if (data) {
                    availableLanguages = data;
                    // Languages fetched successfully, stop trying
                    clearInterval(fetchLanguagesInterval);
                }
            });
        };

        // Fetch languages repeatedly until successful
        const fetchLanguagesInterval = setInterval(fetchLanguages, 5000);
        fetchLanguages();
    };

    const handleTranslate = (id) => {
        if(targetLanguage) {
            const url = selectedSource == 'main'
                ? `${CLIENT_API_HOST}/api/translate/${id}/${targetLanguage}`
                : `${CLIENT_API_HOST}/api/subtitles/${id}/${selectedSource.split(':')[1]}/${targetLanguage}`;
            fetch(url)
            .then(() => toast.success('Translation started!'))
            .catch(error => {
                console.error(error);
                toast.error('Error translating text!')
            });
        }
    }


    onMount(async () => {
        await getAvailableLangs();
    });
</script>

<dialog id="modalTranslation" class="modal">
    <form method="dialog" class="flex flex-col items-center justify-center modal-box">
        <button class="absolute btn btn-sm btn-circle btn-ghost right-2 top-2">✕</button>
        {#if tr}
            <h1 class="pb-2 mt-2 font-bold text-center">
                Translate
            </h1>
            <div>
                <div class="w-full max-w-xs form-control">
                    <label for="source-track" class="label">
                      <span class="label-text">Source text</span>
                    </label>
                    <select bind:value={selectedSource} name="source-track" class="select select-bordered">
                      {#each sourceOptions as source}
                        <option value={source.value}>{source.label}</option>
                      {/each}
                    </select>
                </div>
                <!-- Language picker -->
                <div class="w-full max-w-xs form-control">
                    <label for="target-lan" class="label">
                      <span class="label-text">Target languages for {sourceLanguage || 'selected source'}</span>
                    </label>
                    <select bind:value={targetLanguage} name="target-lan" class="select select-bordered">
                      <option disabled selected>Pick one</option>
                      {#each targetLanguages as t}
                        <option value="{t}">{t}</option>
                      {/each}
                    </select>
                </div>
                <!-- End language picker-->
                <button on:click={handleTranslate(tr.id)} class="mt-5 btn btn-active btn-primary">Translate</button>
            </div>
        {/if}
    </form>
    <form method="dialog" class="modal-backdrop">
        <button>close</button>
    </form>
</dialog>
