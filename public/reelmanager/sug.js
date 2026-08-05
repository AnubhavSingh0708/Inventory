/**
 * sugge.js
 * A simple, dependency-free suggestion/autocomplete library.
 * Version 1.0.0
 */
(function(window) {
    'use strict';

    // Main function exposed to the global scope
    function sugge(targetId, placeAtBottom, options) {
        const targetElement = document.getElementById(targetId);

        if (!targetElement) {
            console.error(`[sugge.js] Element with ID "${targetId}" not found.`);
            return;
        }

        let suggestionsContainer = null;
        let activeSuggestionIndex = -1;
        
        const isInput = targetElement.tagName === 'INPUT' || targetElement.tagName === 'TEXTAREA';

        // --- Core Logic ---

        /**
         * Filters, sorts, and displays suggestions based on the current input.
         */
        const updateSuggestions = () => {
            const inputText = (isInput ? targetElement.value : targetElement.textContent).trim().toLowerCase();

            // Hide if input is empty
            if (!inputText) {
                hideSuggestions();
                return;
            }

            const matchedOptions = options
                .filter(option => option.toLowerCase().includes(inputText))
                .sort(); // Lexicographical sort

            // Hide if no matches or if there's an exact match
            if (matchedOptions.length === 0 || (matchedOptions.length === 1 && matchedOptions[0].toLowerCase() === inputText)) {
                hideSuggestions();
                return;
            }

            createOrUpdateSuggestionsContainer(matchedOptions);
        };

        /**
         * Creates the suggestions container or updates it with new options.
         * @param {string[]} matchedOptions - The array of options to display.
         */
        const createOrUpdateSuggestionsContainer = (matchedOptions) => {
            if (!suggestionsContainer) {
                suggestionsContainer = document.createElement('div');
                suggestionsContainer.className = 'sugge-container';
                document.body.appendChild(suggestionsContainer);
            }
            
            suggestionsContainer.innerHTML = '';
            
            matchedOptions.forEach((option, index) => {
                const optionElement = document.createElement('div');
                optionElement.className = 'sugge-option';
                optionElement.textContent = option;
                optionElement.addEventListener('click', () => {
                    selectSuggestion(index);
                });
                suggestionsContainer.appendChild(optionElement);
            });

            positionSuggestionsContainer();
            showSuggestions();
        };

        /**
         * Positions the suggestions container above or below the target element.
         */
const positionSuggestionsContainer = () => {
            if (!suggestionsContainer) return;
            
            const GAP = 2; // A small gap (in pixels) between the element and the suggestions
            const rect = targetElement.getBoundingClientRect();
            suggestionsContainer.style.left = `${rect.left + window.scrollX}px`;
            suggestionsContainer.style.width = `${rect.width}px`;

            if (placeAtBottom) {
                // Remove the 'above' class if it exists
                suggestionsContainer.classList.remove('sugge-above');
                suggestionsContainer.style.top = `${rect.bottom + window.scrollY + GAP}px`;
            } else {
                // Add a specific class for 'above' positioning
                suggestionsContainer.classList.add('sugge-above');
                // The JS positioning logic remains the same: place the container's top edge
                // where it needs to be. CSS will handle the internal layout.
                suggestionsContainer.style.top = `${rect.top + window.scrollY - suggestionsContainer.offsetHeight - GAP}px`;
            }
        };

        // --- Visibility and Interaction ---

        const showSuggestions = () => {
            if (suggestionsContainer) {
                suggestionsContainer.style.display = 'block';
                activeSuggestionIndex = -1;
            }
        };

        const hideSuggestions = () => {
            if (suggestionsContainer) {
                suggestionsContainer.style.display = 'none';
            }
        };

        /**
         * Selects a suggestion and updates the target element's value.
         * @param {number} index - The index of the selected suggestion.
         */
        const selectSuggestion = (index) => {
            if (!suggestionsContainer || index < 0) return;
            const selectedOption = suggestionsContainer.children[index].textContent;
            
            if (isInput) {
                targetElement.value = selectedOption;
            } else {
                targetElement.textContent = selectedOption;
            }

            hideSuggestions();
        };

        /**
         * Handles keyboard navigation (ArrowUp, ArrowDown, Enter, Escape).
         * @param {KeyboardEvent} event
         */
        const handleKeyDown = (event) => {
            if (!suggestionsContainer || suggestionsContainer.style.display === 'none') return;
            
            const optionsCount = suggestionsContainer.children.length;
            if (optionsCount === 0) return;

            switch (event.key) {
                case 'ArrowDown':
                    event.preventDefault();
                    activeSuggestionIndex = (activeSuggestionIndex + 1) % optionsCount;
                    highlightSuggestion();
                    break;
                case 'ArrowUp':
                    event.preventDefault();
                    activeSuggestionIndex = (activeSuggestionIndex - 1 + optionsCount) % optionsCount;
                    highlightSuggestion();
                    break;
                case 'Enter':
                    event.preventDefault();
                    if (activeSuggestionIndex > -1) {
                        selectSuggestion(activeSuggestionIndex);
                    }
                    break;
                case 'Escape':
                    hideSuggestions();
                    break;
            }
        };

        /**
         * Highlights the currently active suggestion based on `activeSuggestionIndex`.
         */
        const highlightSuggestion = () => {
            Array.from(suggestionsContainer.children).forEach((child, index) => {
                child.classList.toggle('active', index === activeSuggestionIndex);
            });
        };

        // --- Event Listeners ---

        // Main trigger on input/edit
        targetElement.addEventListener('input', updateSuggestions);
        targetElement.addEventListener('focusin', updateSuggestions);
        // Keyboard navigation
        targetElement.addEventListener('keydown', handleKeyDown);
        // Hide when clicking outside
        document.addEventListener('click', (event) => {
            if (!targetElement.contains(event.target) && (!suggestionsContainer || !suggestionsContainer.contains(event.target))) {
                hideSuggestions();
            }
        });
    }

    // Expose the sugge function to the global window object
    window.sugge = sugge;

})(window);