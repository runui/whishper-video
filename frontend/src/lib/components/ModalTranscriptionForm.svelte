<script>
	import { validateURL, CLIENT_API_HOST } from '$lib/utils.js';
	import { env } from '$env/dynamic/public';
	import { uploadProgress } from '$lib/stores';

	import toast from 'svelte-french-toast';

	let errorMessage = '';
	let disableSubmit = true;
	let modelSize = 'small';
	let language = 'auto';
	let sourceType = 'file';
	let sourceUrl = '';
	let localPath = '';
	let localPathEntries = [];
	let localPathLoading = false;
	let localPathError = '';
	let localPathSuggestionsVisible = false;
	let localPathBrowseTimer;
	let skipWhisper = false;
	let fileInput;
	let device = env.PUBLIC_WHISHPER_PROFILE == 'gpu' ? 'cuda' : 'cpu';

	let languages = [
		'auto',
		'ar',
		'be',
		'bg',
		'bn',
		'ca',
		'cs',
		'cy',
		'da',
		'de',
		'el',
		'en',
		'es',
		'fr',
		'it',
		'ja',
		'nl',
		'pl',
		'pt',
		'ru',
		'sk',
		'sl',
		'sv',
		'tk',
		'tr',
		'zh'
	];
	let models = [
		'tiny',
		'tiny.en',
		'base',
		'base.en',
		'small',
		'small.en',
		'medium',
		'medium.en',
		'large-v2',
		'large-v3'
	];
	// Sort the languages
	languages.sort((a, b) => {
		if (a == 'auto') return -1;
		if (b == 'auto') return 1;
		return a.localeCompare(b);
	});

	// Function that sends the data as a form to the backend
	async function sendForm() {
		if (sourceType == 'url' && sourceUrl && !validateURL(sourceUrl)) {
			toast.error('You must enter a valid URL.');
			return;
		}
		if (sourceType == 'local' && localPath && !localPath.startsWith('/')) {
			toast.error('Container file path must be absolute.');
			return;
		}

		if (sourceType == 'file' && (!fileInput || fileInput.files.length == 0)) {
			toast.error('No file selected.');
			return;
		}
		if (sourceType == 'url' && sourceUrl == '') {
			toast.error('No URL entered.');
			return;
		}
		if (sourceType == 'local' && localPath == '') {
			toast.error('No container path entered.');
			return;
		}

		let formData = new FormData();
		formData.append('language', language);
		formData.append('modelSize', modelSize);
		if (device == 'cuda' || device == 'cpu') {
			formData.append('device', device);
		} else {
			formData.append('device', 'cpu');
		}
		formData.append('sourceUrl', sourceType == 'url' ? sourceUrl : '');
		formData.append('localPath', sourceType == 'local' ? localPath : '');
		formData.append('skipWhisper', skipWhisper ? 'true' : 'false');
		if (sourceType == 'file') {
			formData.append('file', fileInput.files[0]);
		}

		return new Promise((resolve, reject) => {
			const xhr = new XMLHttpRequest();

			// Set up progress event listener
			xhr.upload.addEventListener('progress', (event) => {
				if (event.lengthComputable) {
					const percentCompleted = Math.round((event.loaded * 100) / event.total);
					uploadProgress.set(percentCompleted);
				}
			});

			// Set up load event listener
			xhr.addEventListener('load', () => {
				if (xhr.status === 200) {
					resolve(xhr.response);
					toast.success('Success!');
				} else {
					reject(xhr.statusText);
					toast.error('Upload failed');
				}
				uploadProgress.set(0); // Reset progress after completion
			});

			// Set up error event listener
			xhr.addEventListener('error', () => {
				reject(xhr.statusText);
				toast.error('An error occurred during upload');
				uploadProgress.set(0); // Reset progress on error
			});

			xhr.open('POST', `${CLIENT_API_HOST}/api/transcriptions`);
			xhr.send(formData);
		});

		// Set file and sourceUrl to empty
		sourceUrl = '';
		localPath = '';
		localPathEntries = [];
		if (fileInput) fileInput.value = '';
		uploadProgress.set(0);

		toast.success('Success!');
	}

	// Reactive statement
	function localPathDirectory(path) {
		if (!path || !path.startsWith('/')) return '';
		if (path.endsWith('/')) return path;
		const index = path.lastIndexOf('/');
		return index <= 0 ? '/' : path.slice(0, index + 1);
	}

	function localPathPrefix(path) {
		if (!path || path.endsWith('/')) return '';
		const index = path.lastIndexOf('/');
		return index < 0 ? path : path.slice(index + 1).toLowerCase();
	}

	async function browseLocalPath() {
		if (sourceType != 'local' || !localPath.startsWith('/')) {
			localPathEntries = [];
			localPathError = '';
			localPathSuggestionsVisible = false;
			return;
		}

		const directory = localPathDirectory(localPath);
		if (!directory) return;
		localPathLoading = true;
		localPathError = '';
		try {
			const response = await fetch(`${CLIENT_API_HOST}/api/files?path=${encodeURIComponent(directory)}`);
			if (!response.ok) throw new Error(`HTTP ${response.status}`);
			const data = await response.json();
			const prefix = localPathPrefix(localPath);
			localPathEntries = (data.entries || []).filter((entry) => {
				if (!prefix) return true;
				return entry.name.toLowerCase().startsWith(prefix);
			}).sort((a, b) => {
				if (a.isDir != b.isDir) return a.isDir ? -1 : 1;
				return a.name.localeCompare(b.name);
			});
			localPathSuggestionsVisible = true;
		} catch (error) {
			console.error(error);
			localPathEntries = [];
			localPathError = '';
			localPathSuggestionsVisible = false;
		} finally {
			localPathLoading = false;
		}
	}

	function scheduleLocalPathBrowse() {
		localPathSuggestionsVisible = true;
		clearTimeout(localPathBrowseTimer);
		localPathBrowseTimer = setTimeout(browseLocalPath, 250);
	}

	function hideLocalPathSuggestions() {
		localPathSuggestionsVisible = false;
	}

	function handleLocalPathKeydown(event) {
		if (event.key == 'Escape') {
			hideLocalPathSuggestions();
		}
	}

	function selectLocalPath(entry) {
		localPath = entry.path;
		if (entry.isDir) {
			localPathSuggestionsVisible = true;
			scheduleLocalPathBrowse();
		} else {
			localPathEntries = [];
			hideLocalPathSuggestions();
		}
	}

	$: if (sourceType == 'url' && sourceUrl && !validateURL(sourceUrl)) {
		errorMessage = 'Enter a valid URL';
		disableSubmit = true;
	} else if (sourceType == 'local' && localPath && !localPath.startsWith('/')) {
		errorMessage = 'Enter an absolute container file path';
		disableSubmit = true;
	} else {
		errorMessage = '';
		disableSubmit = false;
	}
</script>

<dialog id="modalNewTranscription" class="modal">
	<form method="dialog" class="modal-box">
		<button class="absolute btn btn-sm btn-circle btn-ghost right-2 top-2">✕</button>
		{#if errorMessage != ''}
			<div class="alert alert-error">
				<svg
					xmlns="http://www.w3.org/2000/svg"
					class="w-6 h-6 stroke-current shrink-0"
					fill="none"
					viewBox="0 0 24 24"
					><path
						stroke-linecap="round"
						stroke-linejoin="round"
						stroke-width="2"
						d="M10 14l2-2m0 0l2-2m-2 2l-2-2m2 2l2 2m7-2a9 9 0 11-18 0 9 9 0 0118 0z"
					/></svg
				>
				<span>{errorMessage}</span>
			</div>
		{/if}
		<div class="mt-0 space-y-2">
			<div class="tabs tabs-boxed">
				<button type="button" class:tab-active={sourceType == 'file'} class="tab" on:click={() => { sourceType = 'file'; hideLocalPathSuggestions(); }}>Upload</button>
				<button type="button" class:tab-active={sourceType == 'url'} class="tab" on:click={() => { sourceType = 'url'; hideLocalPathSuggestions(); }}>URL</button>
				<button type="button" class:tab-active={sourceType == 'local'} class="tab" on:click={() => { sourceType = 'local'; browseLocalPath(); }}>Container path</button>
			</div>

			{#if sourceType == 'file'}
			<div class="relative w-full max-w-xs form-control">
				<label for="file" class="label">
					<span class="label-text">Pick a file</span>
				</label>
				<input
					name="file"
					bind:this={fileInput}
					type="file"
					class="w-full max-w-xs file-input file-input-sm file-input-bordered file-input-primary"
				/>
			</div>
			{/if}

			{#if sourceType == 'url'}
			<div class="w-full max-w-xs form-control">
				<label for="sourceUrl" class="label">
					<span class="label-text">Video URL</span>
				</label>
				<input
					name="sourceUrl"
					bind:value={sourceUrl}
					type="text"
					placeholder="Paste a video page or direct media link"
					class="w-full max-w-xs input input-sm input-bordered input-primary"
				/>
			</div>
			{/if}

			{#if sourceType == 'local'}
			<div class="w-full max-w-xs form-control">
				<label for="localPath" class="label">
					<span class="label-text">Container file path</span>
				</label>
				<div class="relative w-full max-w-xs">
					<input
						name="localPath"
						bind:value={localPath}
						type="text"
						placeholder="/media/show/episode.mkv"
						class="w-full max-w-xs input input-sm input-bordered input-primary"
						on:input={scheduleLocalPathBrowse}
						on:focus={browseLocalPath}
						on:keydown={handleLocalPathKeydown}
					/>
					{#if localPathSuggestionsVisible && (localPathLoading || localPathError || localPathEntries.length > 0)}
						<div class="absolute left-0 right-0 top-full z-50 mt-1 max-h-56 overflow-auto rounded border border-base-300 bg-base-100 shadow-xl">
						{#if localPathLoading}
							<div class="flex items-center gap-2 px-3 py-2 text-sm opacity-70">
								<span class="loading loading-spinner loading-sm" />
								<span>Loading...</span>
							</div>
						{:else if localPathError}
							<p class="px-3 py-2 text-sm text-error">{localPathError}</p>
						{:else}
						{#each localPathEntries as entry}
							<button type="button" class="flex w-full items-center justify-between px-3 py-2 text-left text-sm hover:bg-base-200" on:click={() => selectLocalPath(entry)}>
								<span>{entry.isDir ? '📁' : '📄'} {entry.name}</span>
								{#if !entry.isDir}<span class="text-xs opacity-60">{entry.size} B</span>{/if}
							</button>
						{/each}
						{/if}
						</div>
					{/if}
				</div>
				<p class="text-xs opacity-70">
					Path must be readable inside the Whishper container.
				</p>
			</div>
			{/if}
		</div>

		<div class="mb-0 divider" />
		<div class="form-control">
			<label class="justify-start gap-3 cursor-pointer label">
				<input type="checkbox" bind:checked={skipWhisper} class="checkbox checkbox-primary" />
				<span class="label-text">
					Only extract embedded subtitle tracks, skip Whisper transcription
				</span>
			</label>
			{#if skipWhisper}
				<p class="text-xs opacity-70">
					The task will read video subtitle tracks only. If no embedded subtitles exist, no audio transcription will be generated.
				</p>
			{/if}
		</div>

		<div class="mb-0 divider" />
		<!-- Whisper Configuration -->
		<div class="flex space-x-4">
			<div class="w-full max-w-xs form-control">
				<label for="modelSize" class="label">
					<span class="label-text">Whisper model</span>
				</label>
				<select name="modelSize" bind:value={modelSize} class="select select-bordered" disabled={skipWhisper}>
					{#each models as m}
						<option value={m}>{m}</option>
					{/each}
				</select>
			</div>

			<div class="w-full max-w-xs form-control">
				<label for="language" class="label">
					<span class="label-text">Language</span>
				</label>
				<select name="language" bind:value={language} class="select select-bordered">
					{#each languages as l}
						<option value={l}>{l}</option>
					{/each}
				</select>
			</div>

			<div class="w-full max-w-xs form-control">
				<label for="language" class="label">
					<span class="label-text">Device</span>
				</label>
				<select name="device" bind:value={device} class="select select-bordered" disabled={skipWhisper}>
					{#if env.PUBLIC_WHISHPER_PROFILE == 'gpu'}
						<option selected value="cuda">GPU</option>
						<option value="cpu">CPU</option>
					{:else}
						<option selected value="cpu">CPU</option>
						<option disabled value="cuda">GPU</option>
					{/if}
				</select>
			</div>
		</div>

		<div class="mb-0 divider" />
		<!--Actions-->
		<button class="btn btn-wide btn-primary" on:click={sendForm} disabled={disableSubmit}
			>Start</button
		>
	</form>
	<form method="dialog" class="modal-backdrop">
		<button>close</button>
	</form>
</dialog>
