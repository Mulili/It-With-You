export namespace ui {
	
	export class HistoryItem {
	    id: string;
	    role: string;
	    text: string;
	    at: number;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new HistoryItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.role = source["role"];
	        this.text = source["text"];
	        this.at = source["at"];
	        this.status = source["status"];
	    }
	}

}

