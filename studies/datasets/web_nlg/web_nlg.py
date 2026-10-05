import json
import os
import datasets

_CITATION = """\
@inproceedings{castro-ferreira20:bilin-bi-direc-webnl-shared,
  title={The 2020 Bilingual, Bi-Directional WebNLG+ Shared Task Overview and Evaluation Results (WebNLG+ 2020)},
  author={Castro Ferreira, Thiago and
                  Gardent, Claire and
		  Ilinykh, Nikolai and
		  van der Lee, Chris and
		  Mille, Simon and
		  Moussallem, Diego and
		  Shimorina, Anastasia},
  booktitle = {Proceedings of the 3rd WebNLG Workshop on Natural Language Generation from the Semantic Web (WebNLG+ 2020)},
    pages = "55--76",
  year = 	 2020,
  address = 	 {Dublin, Ireland (Virtual)},
  publisher = {Association for Computational Linguistics}}
"""

_DESCRIPTION = """\
WebNLG is a bi-lingual dataset (English, Russian) of parallel DBpedia triple sets
and short texts that cover about 450 different DBpedia properties. The WebNLG data
was originally created to promote the development of RDF verbalisers able to
generate short text and to handle micro-planning (i.e., sentence segmentation and
ordering, referring expression generation, aggregation); the goal of the task is
to generate texts starting from 1 to 7 input triples which have entities in common
(so the input is actually a connected Knowledge Graph). The dataset contains about
17,000 triple sets and 45,000 crowdsourced texts in English, and 7,000 triples sets
and 19,000 crowdsourced texts in Russian. A challenging test set section with
entities and/or properties that have not been seen at training time is available.
"""

_LANG = ["en", "ru"]
_URLs = {
    "en": {
        "train": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_web_nlg/webnlg_en_train.json",
        "validation": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_web_nlg/webnlg_en_val.json",
        "test": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_web_nlg/webnlg_en_test.json",
        "challenge_set": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_challenge_sets/web_nlg_en.zip",
    },
    "ru": {
        "train": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_web_nlg/webnlg_ru_train.json",
        "validation": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_web_nlg/webnlg_ru_val.json",
        "test": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_web_nlg/webnlg_ru_test.json",
        "challenge_set": "https://storage.googleapis.com/huggingface-nlp/datasets/gem/gem_challenge_sets/web_nlg_ru.zip",
    },
}


class WebNLG(datasets.GeneratorBasedBuilder):
    BUILDER_CONFIGS = [
        datasets.BuilderConfig(
            name=lang,
            version=datasets.Version("1.0.0"),
            description="",
        )
        for lang in _LANG
    ]

    def _info(self):
        return datasets.DatasetInfo(
            description=_DESCRIPTION,
            features=datasets.Features(
                {
                    "gem_id": datasets.Value("string"),
                    "gem_parent_id": datasets.Value("string"),
                    "input": [datasets.Value("string")],
                    "target": datasets.Value("string"),  # single target for train
                    "references": [datasets.Value("string")],
                    "category": datasets.Value("string"),
                    "webnlg_id": datasets.Value("string"),
                }
            ),
            supervised_keys=None,
            homepage="https://webnlg-challenge.loria.fr/challenge_2020/",
            citation=_CITATION,
        )

    def _split_generators(self, dl_manager):
        """Returns SplitGenerators."""
        dl_dir = dl_manager.download_and_extract(_URLs[self.config.name])
        lang = str(self.config.name)
        challenge_sets = [
            ("challenge_train_sample", f"train_web_nlg_{lang}_RandomSample500.json"),
            (
                "challenge_validation_sample",
                f"validation_web_nlg_{lang}_RandomSample500.json",
            ),
            (
                "challenge_test_scramble",
                f"test_web_nlg_{lang}_ScrambleInputStructure500.json",
            ),
        ]
        if lang == "en":
            challenge_sets += [
                (
                    "challenge_test_numbers",
                    f"test_web_nlg_{lang}_replace_numbers_500.json",
                )
            ]
        return [
            datasets.SplitGenerator(
                name=spl, gen_kwargs={"filepath": dl_dir[spl], "split": spl}
            )
            for spl in ["train", "validation", "test"]
        ] + [
            datasets.SplitGenerator(
                name=challenge_split,
                gen_kwargs={
                    "filepath": os.path.join(
                        dl_dir["challenge_set"], f"web_nlg_{self.config.name}", filename
                    ),
                    "split": challenge_split,
                },
            )
            for challenge_split, filename in challenge_sets
        ]

    def _generate_examples(self, filepath, split, filepaths=None, lang=None):
        """Yields examples."""
        if "challenge" in split:
            exples = json.load(open(filepath, encoding="utf-8"))
            if isinstance(exples, dict):
                assert len(exples) == 1, "multiple entries found"
                exples = list(exples.values())[0]
            for id_, exple in enumerate(exples):
                if len(exple) == 0:
                    continue
                exple["gem_parent_id"] = exple["gem_id"]
                exple["gem_id"] = f"web_nlg_{self.config.name}-{split}-{id_}"
                yield id_, exple
        else:
            with open(filepath, encoding="utf-8") as f:
                examples = json.load(f)
                id_ = -1
                for example in examples["values"]:
                    if split == "train":
                        for target in example["target"]:
                            id_ += 1
                            yield id_, {
                                "gem_id": f"web_nlg_{self.config.name}-{split}-{id_}",
                                "gem_parent_id": f"web_nlg_{self.config.name}-{split}-{id_}",
                                "input": example["input"],
                                "target": target,
                                "references": []
                                if split == "train"
                                else example["target"],
                                "category": example["category"],
                                "webnlg_id": example["webnlg-id"],
                            }
                    else:
                        id_ += 1
                        yield id_, {
                            "gem_id": f"web_nlg_{self.config.name}-{split}-{id_}",
                            "gem_parent_id": f"web_nlg_{self.config.name}-{split}-{id_}",
                            "input": example["input"],
                            "target": example["target"][0]
                            if len(example["target"]) > 0
                            else "",
                            "references": example["target"],
                            "category": example["category"],
                            "webnlg_id": example["webnlg-id"],
                        }
